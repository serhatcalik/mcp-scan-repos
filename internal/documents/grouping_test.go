package documents

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gitlab-doc-platform/internal/evidence"
)

func TestEndpointsGroupedByNamedRoutePrefix(t *testing.T) {
	s := evidence.Snapshot{Evidence: []evidence.Item{
		{Category: "api", Type: "route", Value: "GET /api/v1/appointments/:id", SourceFile: "internal/appointmentapi/routes.go", LineStart: 10, RelatedComponents: []string{"internal/appointmentapi"}, Confidence: evidence.Verified},
		{Category: "api", Type: "route", Value: "POST /api/v1/appointments", SourceFile: "internal/appointmentapi/routes.go", LineStart: 11, RelatedComponents: []string{"internal/appointmentapi"}, Confidence: evidence.Verified},
		{Category: "api", Type: "route", Value: "GET /api/v1/users", SourceFile: "internal/userapi/routes.go", Confidence: evidence.Verified},
		{Category: "api", Type: "handler", Value: "GetAppointment", SourceFile: "internal/appointmentapi/service.go", RelatedComponents: []string{"internal/appointmentapi"}, Confidence: evidence.Verified},
		{Category: "data", Type: "model_field", Value: "Appointment.ID", SourceFile: "models.go", Confidence: evidence.Verified},
	}}
	for i := range s.Evidence {
		s.Evidence[i].ID = fmt.Sprintf("item-%d", i)
	}
	d := appDiagram(s)
	if strings.Count(d, "subgraph ") != 2 || !strings.Contains(d, "Appointments API - /api/v1/appointments") || !strings.Contains(d, "Users API - /api/v1/users") {
		t.Fatalf("missing named groups: %s", d)
	}
	if !strings.Contains(d, `"Endpoints - 2 endpoint declarations"`) || !strings.Contains(d, "source: internal/appointmentapi/routes.go:10") || strings.Contains(d, "GetAppointment") || strings.Contains(d, "Appointment.ID") || strings.Contains(d, "GET /api/v1/appointments/:id") {
		t.Fatalf("endpoint labels or provenance: %s", d)
	}
	if strings.Contains(d, ` - internal/appointmentapi/routes.go"`) {
		t.Fatal("filenames clutter rendered endpoint labels")
	}
	for i, j := 0, len(s.Evidence)-1; i < j; i, j = i+1, j-1 {
		s.Evidence[i], s.Evidence[j] = s.Evidence[j], s.Evidence[i]
	}
	if appDiagram(s) != d {
		t.Fatal("grouping is not deterministic")
	}
	if _, err := NewGenerator(mockProvider{}).Generate(context.Background(), s, Metadata{}); err != nil {
		t.Fatalf("grouped artifacts failed validation: %v", err)
	}
}

func TestMermaidGroupValidationRejectsMalformedGroups(t *testing.T) {
	a, err := NewGenerator(mockProvider{}).Generate(context.Background(), evidence.Snapshot{}, Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		"  subgraph g0[\"API\"]\n  n0[\"GET /\"]\n",
		"  end\n",
		"  direction TB\n",
		"  subgraph g0[\"API\"]\n  subgraph g1[\"Nested\"]\n  end\n  end\n",
		"  subgraph g0[\"API\"]\n  end\n  subgraph g0[\"Duplicate\"]\n  end\n",
	} {
		a.Diagrams["application-architecture.mmd"] = "flowchart LR\n" + body
		if _, err := ValidateArtifacts(a); err == nil {
			t.Fatalf("accepted invalid group: %s", body)
		}
	}
}

func TestSpringAnnotationsStayTogetherWithoutInventingPaths(t *testing.T) {
	s := evidence.Snapshot{Evidence: []evidence.Item{
		{Category: "api", Type: "route_annotation", Value: "RequestMapping /appointments", SourceFile: "AppointmentController.java", RelatedComponents: []string{"AppointmentController"}, Confidence: evidence.Verified},
		{Category: "api", Type: "route_annotation", Value: "GetMapping /{id}", SourceFile: "AppointmentController.java", RelatedComponents: []string{"AppointmentController"}, Confidence: evidence.Verified},
	}}
	d := appDiagram(s)
	if strings.Count(d, "subgraph ") != 1 || !strings.Contains(d, "AppointmentController") || !strings.Contains(d, "2 mapping declarations") || strings.Contains(d, "/appointments/{id}") {
		t.Fatalf("annotation grouping should not claim composed paths: %s", d)
	}
}
