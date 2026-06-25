package gitlab

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/andrewwormald/everflow/internal/provider"
)

// header is a small helper to build an http.Header with one entry.
func header(name, value string) http.Header {
	h := http.Header{}
	h.Set(name, value)
	return h
}

func TestNormaliseEvent_UnknownHeader(t *testing.T) {
	p := &Provider{}
	_, err := p.NormaliseEvent(header("X-Gitlab-Event", "Push Hook"), []byte(`{}`))
	var ignore provider.ErrIgnore
	if !errors.As(err, &ignore) {
		t.Fatalf("want ErrIgnore for unsubscribed event, got %T %v", err, err)
	}
}

// --- Note Hook ---

func TestNormaliseEvent_NoteHook_MR(t *testing.T) {
	body := []byte(`{
		"object_kind": "note",
		"user": {"id": 42, "name": "Andrew", "username": "andreww", "email": "a@example.com"},
		"project": {"id": 7, "path_with_namespace": "acme/example"},
		"object_attributes": {
			"id": 12345,
			"note": "please rename this method",
			"noteable_type": "MergeRequest"
		},
		"merge_request": {
			"iid": 1234,
			"source_branch": "wf-abc",
			"url": "https://gitlab.com/acme/example/-/merge_requests/1234"
		}
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-Gitlab-Event", "Note Hook"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Kind != provider.EventNoteAdded {
		t.Errorf("Kind: want EventNoteAdded, got %v", ev.Kind)
	}
	if ev.ProjectID != "acme/example" {
		t.Errorf("ProjectID: want acme/example, got %q", ev.ProjectID)
	}
	if ev.MR.IID != 1234 {
		t.Errorf("MR.IID: want 1234, got %d", ev.MR.IID)
	}
	if ev.Author.Handle != "andreww" {
		t.Errorf("Author.Handle: want andreww, got %q", ev.Author.Handle)
	}
	if !strings.Contains(ev.Note.Body, "rename this method") {
		t.Errorf("Note.Body: missing comment text, got %q", ev.Note.Body)
	}
}

func TestNormaliseEvent_NoteHook_OnIssue_Ignored(t *testing.T) {
	body := []byte(`{
		"object_kind": "note",
		"user": {"id": 42, "username": "x"},
		"project": {"id": 7, "path_with_namespace": "p/q"},
		"object_attributes": {"id": 1, "note": "x", "noteable_type": "Issue"}
	}`)
	p := &Provider{}
	_, err := p.NormaliseEvent(header("X-Gitlab-Event", "Note Hook"), body)
	var ignore provider.ErrIgnore
	if !errors.As(err, &ignore) {
		t.Fatalf("want ErrIgnore for issue note, got %T %v", err, err)
	}
}

// --- Pipeline Hook ---

func TestNormaliseEvent_PipelineHook_Success(t *testing.T) {
	body := []byte(`{
		"object_kind": "pipeline",
		"user": {"id": 42, "username": "andreww"},
		"project": {"id": 7, "path_with_namespace": "acme/example"},
		"object_attributes": {"id": 999, "status": "success", "ref": "wf-abc"},
		"merge_request": {"iid": 1234, "source_branch": "wf-abc", "url": "https://x/y/-/merge_requests/1234"},
		"builds": [
			{"id": 1, "name": "lint", "stage": "test", "status": "success"},
			{"id": 2, "name": "go-test", "stage": "test", "status": "success"}
		]
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-Gitlab-Event", "Pipeline Hook"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Kind != provider.EventPipelineSucceeded {
		t.Errorf("Kind: want EventPipelineSucceeded, got %v", ev.Kind)
	}
	if ev.Pipeline.ID != 999 {
		t.Errorf("Pipeline.ID: want 999, got %d", ev.Pipeline.ID)
	}
	if len(ev.Pipeline.FailedJobs) != 0 {
		t.Errorf("FailedJobs: want empty on success, got %d entries", len(ev.Pipeline.FailedJobs))
	}
}

func TestNormaliseEvent_PipelineHook_Failed_PopulatesFailedJobs(t *testing.T) {
	body := []byte(`{
		"object_kind": "pipeline",
		"user": {"id": 42, "username": "andreww"},
		"project": {"path_with_namespace": "acme/example"},
		"object_attributes": {"id": 999, "status": "failed", "ref": "wf-abc"},
		"merge_request": {"iid": 1234, "source_branch": "wf-abc", "url": "x"},
		"builds": [
			{"id": 1, "name": "lint", "stage": "test", "status": "success"},
			{"id": 2, "name": "go-test 3/5", "stage": "test", "status": "failed"},
			{"id": 3, "name": "danger", "stage": "test", "status": "failed"}
		]
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-Gitlab-Event", "Pipeline Hook"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Kind != provider.EventPipelineFailed {
		t.Errorf("Kind: want EventPipelineFailed, got %v", ev.Kind)
	}
	if got, want := len(ev.Pipeline.FailedJobs), 2; got != want {
		t.Fatalf("FailedJobs: want %d entries, got %d", want, got)
	}
	if ev.Pipeline.FailedJobs[0].Name != "go-test 3/5" {
		t.Errorf("FailedJobs[0].Name: got %q", ev.Pipeline.FailedJobs[0].Name)
	}
}

func TestNormaliseEvent_PipelineHook_NotForMR_Ignored(t *testing.T) {
	body := []byte(`{
		"object_kind": "pipeline",
		"project": {"path_with_namespace": "p/q"},
		"object_attributes": {"id": 1, "status": "success", "ref": "main"}
	}`)
	p := &Provider{}
	_, err := p.NormaliseEvent(header("X-Gitlab-Event", "Pipeline Hook"), body)
	var ignore provider.ErrIgnore
	if !errors.As(err, &ignore) {
		t.Fatalf("want ErrIgnore for pipeline not tied to MR, got %T %v", err, err)
	}
}

// --- Merge Request Hook ---

func TestNormaliseEvent_MRHook_Merged(t *testing.T) {
	body := []byte(`{
		"object_kind": "merge_request",
		"user": {"id": 42, "username": "andreww"},
		"project": {"path_with_namespace": "acme/example"},
		"object_attributes": {
			"iid": 1234,
			"action": "merge",
			"state": "merged",
			"source_branch": "wf-abc",
			"url": "https://gitlab.com/acme/example/-/merge_requests/1234"
		}
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-Gitlab-Event", "Merge Request Hook"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Kind != provider.EventMRMerged {
		t.Errorf("Kind: want EventMRMerged, got %v", ev.Kind)
	}
	if ev.MR.IID != 1234 {
		t.Errorf("MR.IID: want 1234, got %d", ev.MR.IID)
	}
}

func TestNormaliseEvent_MRHook_Open_Ignored(t *testing.T) {
	body := []byte(`{
		"object_kind": "merge_request",
		"user": {"username": "andreww"},
		"project": {"path_with_namespace": "p/q"},
		"object_attributes": {"iid": 1, "action": "open", "state": "opened"}
	}`)
	p := &Provider{}
	_, err := p.NormaliseEvent(header("X-Gitlab-Event", "Merge Request Hook"), body)
	var ignore provider.ErrIgnore
	if !errors.As(err, &ignore) {
		t.Fatalf("want ErrIgnore for self-emitted open, got %T %v", err, err)
	}
}

func TestNormaliseEvent_MRHook_Close_NotMerged(t *testing.T) {
	body := []byte(`{
		"object_kind": "merge_request",
		"user": {"username": "andreww"},
		"project": {"path_with_namespace": "p/q"},
		"object_attributes": {"iid": 5, "action": "close", "state": "closed"}
	}`)
	p := &Provider{}
	ev, err := p.NormaliseEvent(header("X-Gitlab-Event", "Merge Request Hook"), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Kind != provider.EventMRClosed {
		t.Errorf("Kind: want EventMRClosed, got %v", ev.Kind)
	}
}

// --- project ID fallback ---

func TestGitlabProject_idAsString_FallsBackToNumeric(t *testing.T) {
	if g := (gitlabProject{ID: 42, PathWithNamespace: ""}).idAsString(); g != "42" {
		t.Errorf("want %q, got %q", "42", g)
	}
	if g := (gitlabProject{ID: 42, PathWithNamespace: "acme/example"}).idAsString(); g != "acme/example" {
		t.Errorf("want path-with-namespace, got %q", g)
	}
}
