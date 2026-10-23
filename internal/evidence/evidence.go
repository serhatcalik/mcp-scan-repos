package evidence

const (
	AnalyzerVersion            = "0.3.3"
	DocumentationSchemaVersion = "0.6.2"
)

type Confidence string

const (
	Verified    Confidence = "verified"
	Inferred    Confidence = "inferred"
	Unavailable Confidence = "unavailable"
)

type Item struct {
	ID                string     `json:"id"`
	Category          string     `json:"category"`
	Type              string     `json:"type"`
	Value             string     `json:"value"`
	SourceFile        string     `json:"source_file"`
	LineStart         int        `json:"line_start,omitempty"`
	LineEnd           int        `json:"line_end,omitempty"`
	CommitSHA         string     `json:"commit_sha"`
	Verification      string     `json:"verification"`
	Confidence        Confidence `json:"confidence"`
	RelatedComponents []string   `json:"related_components,omitempty"`
}

type Snapshot struct {
	ProjectID string   `json:"project_id"`
	CommitSHA string   `json:"commit_sha"`
	Language  string   `json:"language"`
	Framework string   `json:"framework"`
	Database  string   `json:"database"`
	Evidence  []Item   `json:"evidence"`
	Inventory []File   `json:"inventory,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

type File struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Language string `json:"language,omitempty"`
}
