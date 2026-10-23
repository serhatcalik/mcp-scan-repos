package publication

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gitlab-doc-platform/internal/documents"
	"gitlab-doc-platform/internal/evidence"
)

func revisionLabel(base string, used map[string]bool) string {
	if !used[base] {
		return base
	}
	for revision := 2; ; revision++ {
		label := base + " - Revision " + strconv.Itoa(revision)
		if !used[label] {
			return label
		}
	}
}

// ApplicationVersion prefers an explicit root version file. Otherwise all
// verified root manifest declarations must agree; dependency versions never count.
func ApplicationVersion(items []evidence.Item) string {
	for _, sources := range [][]string{{"VERSION", "version.txt"}, {".gitlab-docs-release.json"}, {"package.json", "pom.xml"}} {
		versions := map[string]bool{}
		for _, e := range items {
			if e.Type != "application_version" || e.Category != "application" || e.Confidence != evidence.Verified || !evidence.ValidApplicationVersion(e.Value) || documents.RedactText(e.Value) != e.Value {
				continue
			}
			if len(sources) > 0 && sources[0] == "package.json" && evidence.GoApplicationVersionSource(e.SourceFile) {
				versions[e.Value] = true
			}
			for _, source := range sources {
				if e.SourceFile == source {
					versions[e.Value] = true
				}
			}
		}
		if len(versions) > 1 {
			return ""
		}
		for version := range versions {
			return version
		}
	}
	return ""
}

// SnapshotLabel must run under the provider's publication session lock. Freeze
// the chosen label before remote writes so retries cannot rename a snapshot.
func SnapshotLabel(ctx context.Context, conn *pgxpool.Conn, id int64, job, project, target, dateLabel, titleScope string) (string, error) {
	var label string
	if err := conn.QueryRow(ctx, `select coalesce(details->>'snapshot_label','') from publication_records where id=$1`, id).Scan(&label); err != nil {
		return "", errors.New("load publication snapshot label failed")
	}
	if label != "" {
		if len(label) > 200 || strings.ContainsAny(label, "\r\n") || documents.RedactText(label) != label {
			return "", errors.New("invalid stored publication snapshot label")
		}
		return label, nil
	}
	var legacy bool
	switch {
	case strings.HasPrefix(target, "notion:"):
		if err := conn.QueryRow(ctx, `select exists(select 1 from notion_pages where parent_page_id=$1 and project_id=$2 and job_id=$3 and kind='snapshot')`, strings.TrimPrefix(target, "notion:"), project, job).Scan(&legacy); err != nil {
			return "", errors.New("load existing publication snapshot failed")
		}
	case strings.HasPrefix(target, "confluence:"):
		if err := conn.QueryRow(ctx, `select exists(select 1 from confluence_pages where destination_key=$1 and project_id=$2 and job_id=$3 and kind='snapshot')`, strings.TrimPrefix(target, "confluence:"), project, job).Scan(&legacy); err != nil {
			return "", errors.New("load existing publication snapshot failed")
		}
	default:
		return "", errors.New("unsupported publication target")
	}
	label = dateLabel
	if legacy {
		titleScope = ""
	}
	if !legacy {
		rows, err := conn.Query(ctx, `select category,type,value,source_file,confidence from evidence_items where job_id=$1 and type='application_version'`, job)
		if err != nil {
			return "", errors.New("load application version failed")
		}
		var items []evidence.Item
		for rows.Next() {
			var e evidence.Item
			if err := rows.Scan(&e.Category, &e.Type, &e.Value, &e.SourceFile, &e.Confidence); err != nil {
				rows.Close()
				return "", errors.New("read application version failed")
			}
			items = append(items, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", errors.New("read application version failed")
		}
		if version := ApplicationVersion(items); version != "" {
			label = "Documentation - " + version
		}
		if label != dateLabel || titleScope != "" {
			var rows pgx.Rows
			var err error
			if titleScope != "" {
				rows, err = conn.Query(ctx, `select details->>'snapshot_label' from publication_records where id<>$1
and details->>'snapshot_title_scope'=$2 and
(details->>'snapshot_label'=$3 or starts_with(details->>'snapshot_label',$3||' - Revision '))`, id, titleScope, label)
			} else {
				rows, err = conn.Query(ctx, `select p.details->>'snapshot_label' from publication_records p join documentation_jobs j on j.id=p.job_id
where p.target=$1 and j.project_id=$2 and p.id<>$3 and
(p.details->>'snapshot_label'=$4 or starts_with(p.details->>'snapshot_label',$4||' - Revision '))`, target, project, id, label)
			}
			if err != nil {
				return "", errors.New("load publication version revisions failed")
			}
			used := map[string]bool{}
			for rows.Next() {
				var value string
				if err := rows.Scan(&value); err != nil {
					rows.Close()
					return "", errors.New("read publication version revisions failed")
				}
				used[value] = true
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return "", errors.New("read publication version revisions failed")
			}
			label = revisionLabel(label, used)
		}
	}
	if _, err := conn.Exec(ctx, `update publication_records set details=details||jsonb_build_object('snapshot_label',$2::text,'snapshot_title_scope',$3::text),updated_at=now() where id=$1`, id, label, titleScope); err != nil {
		return "", errors.New("persist publication snapshot label failed")
	}
	return label, nil
}
