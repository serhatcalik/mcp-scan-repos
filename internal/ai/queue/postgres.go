package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gitlab-doc-platform/internal/ai"
)

var (
	ErrFull      = errors.New("inference queue is full; retry the documentation job later")
	ErrLeaseLost = errors.New("inference admission lease lost")
)

type Postgres struct {
	db        *pgxpool.Pool
	pool      string
	poll      time.Duration
	heartbeat time.Duration
	lease     time.Duration
}

func NewPostgres(ctx context.Context, db *pgxpool.Pool, pool string, running, waiting int) (*Postgres, error) {
	if pool == "" || running < 1 || waiting < 1 {
		return nil, errors.New("invalid inference queue configuration")
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `insert into inference_pools(id,max_running,max_waiting) values($1,$2,$3) on conflict(id) do nothing`, pool, running, waiting); err != nil {
		return nil, err
	}
	var currentRunning, currentWaiting int
	if err := tx.QueryRow(ctx, `select max_running,max_waiting from inference_pools where id=$1 for update`, pool).Scan(&currentRunning, &currentWaiting); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `delete from inference_requests where pool_id=$1 and lease_expires_at<=now()`, pool); err != nil {
		return nil, err
	}
	if currentRunning != running || currentWaiting != waiting {
		var count int
		if err := tx.QueryRow(ctx, `select count(*) from inference_requests where pool_id=$1`, pool).Scan(&count); err != nil {
			return nil, err
		}
		if count > 0 {
			return nil, errors.New("drain the inference queue before changing its limits")
		}
		if _, err := tx.Exec(ctx, `update inference_pools set max_running=$2,max_waiting=$3 where id=$1`, pool, running, waiting); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &Postgres{db: db, pool: pool, poll: 500 * time.Millisecond, heartbeat: 20 * time.Second, lease: 90 * time.Second}, nil
}

func (q *Postgres) Acquire(ctx context.Context) (ai.Admission, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return ai.Admission{}, err
	}
	id := hex.EncodeToString(bytes[:])
	if err := q.enqueue(ctx, id); err != nil {
		return ai.Admission{}, err
	}
	admitted := false
	defer func() {
		if !admitted {
			q.remove(id)
		}
	}()
	for {
		running, err := q.tryStart(ctx, id)
		if err != nil {
			return ai.Admission{}, err
		}
		if running {
			break
		}
		timer := time.NewTimer(q.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ai.Admission{}, ctx.Err()
		case <-timer.C:
		}
	}
	permitCtx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(q.heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-permitCtx.Done():
				return
			case <-ticker.C:
				renewCtx, stop := context.WithTimeout(permitCtx, 5*time.Second)
				result, err := q.db.Exec(renewCtx, `update inference_requests set lease_expires_at=now()+($2*interval '1 second') where id=$1 and status='running' and lease_expires_at>now()`, id, q.lease.Seconds())
				stop()
				if err != nil || result.RowsAffected() != 1 {
					cancel(ErrLeaseLost)
					return
				}
			}
		}
	}()
	var once sync.Once
	admitted = true
	return ai.Admission{Context: permitCtx, Release: func() { once.Do(func() { cancel(context.Canceled); <-done; q.remove(id) }) }}, nil
}

func (q *Postgres) enqueue(ctx context.Context, id string) error {
	tx, err := q.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var limit int
	if err := tx.QueryRow(ctx, `select max_waiting from inference_pools where id=$1 for update`, q.pool).Scan(&limit); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from inference_requests where pool_id=$1 and lease_expires_at<=now()`, q.pool); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, `select count(*) from inference_requests where pool_id=$1 and status='waiting'`, q.pool).Scan(&count); err != nil {
		return err
	}
	if count >= limit {
		return ErrFull
	}
	if _, err := tx.Exec(ctx, `insert into inference_requests(id,pool_id,status,lease_expires_at) values($1,$2,'waiting',now()+($3*interval '1 second'))`, id, q.pool, q.lease.Seconds()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (q *Postgres) tryStart(ctx context.Context, id string) (bool, error) {
	tx, err := q.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	var limit int
	if err := tx.QueryRow(ctx, `select max_running from inference_pools where id=$1 for update`, q.pool).Scan(&limit); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `delete from inference_requests where pool_id=$1 and lease_expires_at<=now()`, q.pool); err != nil {
		return false, err
	}
	result, err := tx.Exec(ctx, `update inference_requests set lease_expires_at=now()+($2*interval '1 second') where id=$1 and status='waiting'`, id, q.lease.Seconds())
	if err != nil {
		return false, err
	}
	if result.RowsAffected() != 1 {
		return false, ErrLeaseLost
	}
	var active int
	if err := tx.QueryRow(ctx, `select count(*) from inference_requests where pool_id=$1 and status='running'`, q.pool).Scan(&active); err != nil {
		return false, err
	}
	var first string
	if err := tx.QueryRow(ctx, `select id from inference_requests where pool_id=$1 and status='waiting' order by sequence limit 1`, q.pool).Scan(&first); err != nil {
		return false, err
	}
	start := active < limit && first == id
	if start {
		if _, err := tx.Exec(ctx, `update inference_requests set status='running' where id=$1`, id); err != nil {
			return false, err
		}
	}
	return start, tx.Commit(ctx)
}

func (q *Postgres) remove(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = q.db.Exec(ctx, `delete from inference_requests where id=$1`, id)
}

var _ ai.Gate = (*Postgres)(nil)
