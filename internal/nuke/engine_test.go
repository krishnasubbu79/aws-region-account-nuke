package nuke

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeService struct {
	resources []Resource
	deleteErr map[string]error
	waitErr   map[string]error
	mu        sync.Mutex
	deleted   []string
	waited    []string
}

func (s *fakeService) Name() string { return "fake" }
func (s *fakeService) Discover(context.Context) ([]Resource, error) {
	return s.resources, nil
}
func (s *fakeService) Delete(_ context.Context, resource Resource) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, resource.ID)
	return s.deleteErr[resource.ID]
}
func (s *fakeService) Wait(_ context.Context, resource Resource) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waited = append(s.waited, resource.ID)
	return s.waitErr[resource.ID]
}

func TestSelectorMatchesIDOrAllTags(t *testing.T) {
	resource := Resource{ID: "cache-1", Name: "preview-cache", Tags: map[string]string{
		"Environment": "preview",
		"Owner":       "payments",
	}}

	tests := []struct {
		name     string
		selector Selector
		want     bool
	}{
		{name: "exact ID", selector: Selector{IDs: []string{"cache-1"}}, want: true},
		{name: "name", selector: Selector{IDs: []string{"preview-cache"}}, want: true},
		{name: "all tags", selector: Selector{Tags: map[string]string{"Environment": "preview", "Owner": "payments"}}, want: true},
		{name: "one tag differs", selector: Selector{Tags: map[string]string{"Environment": "preview", "Owner": "analytics"}}, want: false},
		{name: "ID or tags", selector: Selector{IDs: []string{"cache-1"}, Tags: map[string]string{"Owner": "analytics"}}, want: true},
		{name: "empty selector", selector: Selector{}, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.selector.Matches(resource); got != test.want {
				t.Fatalf("Matches() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestProtectedResourceIsNeverDeleted(t *testing.T) {
	service := &fakeService{resources: []Resource{
		{Service: "fake", ID: "ordinary", Name: "ordinary"},
		{Service: "fake", ID: "protected", Name: "protected", Tags: map[string]string{"DoNotDelete": "TRUE"}},
	}}
	engine := Engine{
		Services: []Service{service}, Selector: Selector{IDs: []string{"ordinary", "protected"}},
		Parallel: 2, Wait: true, WaitTimeout: time.Second,
	}

	plan, err := engine.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if plan[0].Status != "planned" || plan[1].Status != "protected" {
		t.Fatalf("unexpected plan statuses: %#v", plan)
	}
	result := engine.Execute(context.Background(), plan)
	if result[0].Status != "deleted" || result[1].Status != "protected" {
		t.Fatalf("unexpected result statuses: %#v", result)
	}
	if len(service.deleted) != 1 || service.deleted[0] != "ordinary" {
		t.Fatalf("deleted resources = %v, want only ordinary", service.deleted)
	}
}

func TestDeleteFailureIsReportedAndNotWaited(t *testing.T) {
	service := &fakeService{
		resources: []Resource{{Service: "fake", ID: "broken"}},
		deleteErr: map[string]error{"broken": errors.New("AWS refused deletion")},
	}
	engine := Engine{
		Services: []Service{service}, Selector: Selector{IDs: []string{"broken"}},
		Parallel: 1, Wait: true, WaitTimeout: time.Second,
	}
	plan, err := engine.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := engine.Execute(context.Background(), plan)

	if result[0].Status != "failed" || result[0].Error != "AWS refused deletion" {
		t.Fatalf("unexpected result: %#v", result[0])
	}
	if len(service.waited) != 0 {
		t.Fatalf("Wait called after failed deletion: %v", service.waited)
	}
}
