package analyzer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"gitlab-doc-platform/internal/evidence"
)

func TestGoASTExtractsRoutesModelsMiddlewareAndEnvironment(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "app.go"), `package app
import (
  "github.com/gin-gonic/gin"
  "gorm.io/gorm"
  "os"
)
type Appointment struct { gorm.Model; PatientID uint }
func routes(r *gin.Engine) {
  api := r.Group("/api")
  api.Use(authenticate)
  api.GET("/appointments", list)
  api.POST("/appointments", create)
  api.DELETE(dynamicPath, remove)
  _ = os.Getenv("DATABASE_PASSWORD")
  // api.GET("/fake", fake)
}
func list(c *gin.Context) {}
`)
	write(t, filepath.Join(root, "notgin.go"), `package app
func unrelated(client *Other) { client.GET("/not-a-route") }
`)
	snap, err := New().Analyze(context.Background(), root, Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"GET /api/appointments": false, "POST /api/appointments": false, "Appointment": false, "Appointment.PatientID": false, "authenticate": false, "DATABASE_PASSWORD": false, "list": false}
	for _, e := range snap.Evidence {
		if _, ok := want[e.Value]; ok {
			want[e.Value] = true
		}
		if strings.Contains(e.Value, "/fake") || strings.Contains(e.Value, "/not-a-route") {
			t.Errorf("false route: %+v", e)
		}
		if e.Value == "dynamic Gin route path unavailable" && e.Confidence != evidence.Unavailable {
			t.Error("dynamic route marked verified")
		}
	}
	for value, found := range want {
		if !found {
			t.Errorf("missing %s", value)
		}
	}
}

func TestConfigurationNeverIncludesSecretValues(t *testing.T) {
	root := t.TempDir()
	secret := "super-secret-database-password"
	write(t, filepath.Join(root, "app.yaml"), `kind: Deployment
metadata:
  name: diet-api
spec:
  replicas: 2
  template:
    spec:
      containers:
        - name: api
          image: registry.example/diet:v1
          env:
            - name: DATABASE_PASSWORD
              value: `+secret+`
            - name: DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: database
                  key: uri
          resources:
            requests:
              cpu: "100m"
          readinessProbe:
            httpGet:
              path: /ready
              port: 8080
---
kind: Secret
metadata:
  name: database
stringData:
  uri: `+secret+`
`)
	write(t, filepath.Join(root, ".env"), "PASSWORD="+secret+"\n")
	write(t, filepath.Join(root, "application.properties"), "spring.datasource.password="+secret+"\n")
	write(t, filepath.Join(root, "Dockerfile"), "FROM alpine:3.22\nENV PASSWORD="+secret+"\nEXPOSE 8080\n")
	snap, err := New().Analyze(context.Background(), root, Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(snap)
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("secret leaked: %s", encoded)
	}
	want := map[string]bool{"Deployment diet-api": false, "database/uri": false, "readinessProbe": false, "registry.example/diet:v1": false, "8080": false, "spring.datasource.password": false}
	for _, e := range snap.Evidence {
		if _, ok := want[e.Value]; ok {
			want[e.Value] = true
		}
	}
	for value, found := range want {
		if !found {
			t.Errorf("missing config evidence %s", value)
		}
	}
}

func TestJavaNodeAndInventory(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "pom.xml"), `<project><artifactId>appointments</artifactId><dependencies><dependency><groupId>org.springframework.boot</groupId><artifactId>spring-boot-starter-data-jpa</artifactId></dependency></dependencies></project>`)
	write(t, filepath.Join(root, "src", "AppointmentController.java"), `@RestController
class AppointmentController {
  @GetMapping("/appointments") void list() {}
  // @PostMapping("/fake")
  String fake = "@GetMapping('/fake') @RestController";
}`)
	write(t, filepath.Join(root, "web", "package.json"), `{"dependencies":{"express":"5","@prisma/client":"6"},"devDependencies":{"typescript":"5"}}`)
	write(t, filepath.Join(root, "web", "routes.ts"), "router.get('/appointments', handler);\n// router.post('/fake', x);\nconst fake=\"router.get('/fake', x)\";\nconst key=process.env.API_KEY;\n")
	write(t, filepath.Join(root, "web", "app", "api", "appointments", "route.ts"), "export async function GET() {}\n")
	write(t, filepath.Join(root, "broken.yaml"), "kind: [malformed")
	write(t, filepath.Join(root, "generated-docs", "old.java"), "@Service class Old {}")
	first, err := New().Analyze(context.Background(), root, Metadata{CommitSHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := New().Analyze(context.Background(), root, Metadata{CommitSHA: "abc"})
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatal("nondeterministic snapshot")
	}
	if len(first.Inventory) != 6 || len(first.Warnings) != 1 {
		t.Fatalf("inventory=%d warnings=%v", len(first.Inventory), first.Warnings)
	}
	want := map[string]bool{"AppointmentController @RestController": false, "GetMapping /appointments": false, "express": false, "API_KEY": false}
	for _, e := range first.Evidence {
		if strings.Contains(e.Value, "/fake") {
			t.Error("comment treated as evidence")
		}
		if _, ok := want[e.Value]; ok {
			want[e.Value] = true
		}
		if e.Type == "route_candidate" && e.Confidence != evidence.Inferred {
			t.Error("unresolved Express receiver marked verified")
		}
	}
	for value, found := range want {
		if !found {
			t.Errorf("missing language evidence %s", value)
		}
	}
}
