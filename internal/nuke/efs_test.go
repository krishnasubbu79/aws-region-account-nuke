package nuke

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// A stateful EFS endpoint exercises real SDK requests and rejects deleting a
// file system while replication or a mount target still exists.
type efsFixture struct {
	mu            sync.Mutex
	replicated    bool
	remoteRegion  string
	remoteOwner   string
	protectedPeer bool
	protectedAP   bool
	denyMountRead bool
	lingerTarget  bool
	deleted       map[string]bool
	targets       map[string]bool
	points        map[string]bool
	mutations     int
}

func newEFSFixture(t *testing.T, selector Selector) (*efsFixture, *EFSService) {
	t.Helper()
	fixture := &efsFixture{deleted: map[string]bool{},
		targets: map[string]bool{"fs-11111111": true, "fs-22222222": true},
		points:  map[string]bool{"fs-11111111": true, "fs-22222222": true}}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(server.Close)
	service := NewEFSService(aws.Config{Region: "ap-south-1", BaseEndpoint: aws.String(server.URL),
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), RetryMaxAttempts: 1}, selector)
	return fixture, service
}

func (f *efsFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	respond := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	fail := func(code string) {
		w.Header().Set("X-Amzn-Errortype", code)
		w.WriteHeader(http.StatusNotFound)
		respond(map[string]string{"ErrorCode": code, "Message": code})
	}
	path := strings.TrimPrefix(r.URL.Path, "/2015-02-01/")
	if r.Method == http.MethodDelete {
		f.mutations++
		switch {
		case strings.HasSuffix(path, "/replication-configuration"):
			if path != "file-systems/fs-11111111/replication-configuration" || r.URL.Query().Get("deletionMode") != "ALL_CONFIGURATIONS" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.replicated = false
		case strings.HasPrefix(path, "mount-targets/"):
			if !f.lingerTarget {
				f.targets[strings.TrimPrefix(path, "mount-targets/mt-")] = false
			}
		case strings.HasPrefix(path, "access-points/"):
			f.points[strings.TrimPrefix(path, "access-points/ap-")] = false
		case strings.HasPrefix(path, "file-systems/"):
			id := strings.TrimPrefix(path, "file-systems/")
			if f.replicated || f.targets[id] || f.points[id] {
				w.WriteHeader(http.StatusConflict)
				respond(map[string]string{"ErrorCode": "FileSystemInUse"})
				return
			}
			f.deleted[id] = true
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch {
	case path == "file-systems":
		var files []map[string]any
		for _, id := range []string{"fs-11111111", "fs-22222222"} {
			if f.deleted[id] || (r.URL.Query().Get("FileSystemId") != "" && r.URL.Query().Get("FileSystemId") != id) {
				continue
			}
			tags := []map[string]string{{"Key": "Environment", "Value": "preview"}}
			if id == "fs-22222222" && f.protectedPeer {
				tags = append(tags, map[string]string{"Key": "DoNotDelete", "Value": "true"})
			}
			files = append(files, map[string]any{"FileSystemId": id, "Name": id, "OwnerId": "123456789012",
				"FileSystemArn": "arn:aws:elasticfilesystem:ap-south-1:123456789012:file-system/" + id, "Tags": tags})
		}
		if len(files) == 0 {
			fail("FileSystemNotFound")
			return
		}
		// Paginate discovery to catch implementations that only read one page.
		if r.URL.Query().Get("FileSystemId") == "" && len(files) == 2 {
			if r.URL.Query().Get("Marker") == "second" {
				respond(map[string]any{"FileSystems": files[1:]})
			} else {
				respond(map[string]any{"FileSystems": files[:1], "NextMarker": "second"})
			}
			return
		}
		respond(map[string]any{"FileSystems": files})
	case path == "file-systems/replication-configurations":
		if !f.replicated {
			fail("ReplicationNotFound")
			return
		}
		region, owner := "ap-south-1", "123456789012"
		if f.remoteRegion != "" {
			region = f.remoteRegion
		}
		if f.remoteOwner != "" {
			owner = f.remoteOwner
		}
		respond(map[string]any{"Replications": []any{map[string]any{
			"SourceFileSystemId": "fs-11111111", "SourceFileSystemRegion": "ap-south-1",
			"SourceFileSystemArn": "arn:aws:elasticfilesystem:ap-south-1:123456789012:file-system/fs-11111111",
			"Destinations":        []any{map[string]any{"FileSystemId": "fs-22222222", "Region": region, "OwnerId": owner}},
		}}})
	case path == "access-points":
		id := r.URL.Query().Get("FileSystemId")
		if point := r.URL.Query().Get("AccessPointId"); point != "" {
			id = strings.TrimPrefix(point, "ap-")
			if !f.points[id] {
				fail("AccessPointNotFound")
				return
			}
		}
		var tags []map[string]string
		if f.protectedAP {
			tags = append(tags, map[string]string{"Key": "DoNotDelete", "Value": "true"})
		}
		respond(map[string]any{"AccessPoints": []any{map[string]any{"AccessPointId": "ap-" + id, "Tags": tags}}})
	case path == "mount-targets":
		if f.denyMountRead {
			w.Header().Set("X-Amzn-Errortype", "AccessDeniedException")
			w.WriteHeader(http.StatusForbidden)
			respond(map[string]string{"ErrorCode": "AccessDeniedException"})
			return
		}
		id := r.URL.Query().Get("FileSystemId")
		if target := r.URL.Query().Get("MountTargetId"); target != "" {
			id = strings.TrimPrefix(target, "mt-")
			if !f.targets[id] {
				fail("MountTargetNotFound")
				return
			}
		}
		respond(map[string]any{"MountTargets": []any{map[string]any{"MountTargetId": "mt-" + id, "LifeCycleState": "available"}}})
	default:
		fail("unexpected path: " + path)
	}
}

func TestEFSDeletesStandaloneAndReplicationPair(t *testing.T) {
	for _, test := range []struct{ replicated, wait bool }{{false, true}, {true, true}, {true, false}} {
		t.Run(fmt.Sprintf("replication=%v/wait=%v", test.replicated, test.wait), func(t *testing.T) {
			selector := Selector{IDs: []string{"fs-11111111", "fs-22222222"}}
			fixture, service := newEFSFixture(t, selector)
			fixture.replicated = test.replicated
			engine := Engine{Services: []Service{service}, Selector: selector, Parallel: 2, Wait: test.wait, WaitTimeout: time.Second}
			plan, err := engine.Plan(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if fixture.mutations != 0 || len(plan) != 2 {
				t.Fatalf("planning mutated resources or missed a file system: %#v", plan)
			}
			result := engine.Execute(context.Background(), plan)
			wantStatus := "delete-submitted"
			if test.wait {
				wantStatus = "deleted"
			}
			for _, resource := range result {
				if resource.Status != wantStatus {
					t.Fatalf("deletion failed: %#v", resource)
				}
			}
			if fixture.replicated || !fixture.deleted["fs-11111111"] || !fixture.deleted["fs-22222222"] {
				t.Fatal("replication or file systems left behind")
			}
		})
	}
}

func TestEFSDoesNotDeleteFileSystemBeforeMountTargetDisappears(t *testing.T) {
	selector := Selector{IDs: []string{"fs-11111111"}}
	fixture, service := newEFSFixture(t, selector)
	fixture.lingerTarget = true
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := service.Delete(ctx, Resource{ID: "fs-11111111"})
	if err == nil || !strings.Contains(err.Error(), "wait for EFS mount target removal") {
		t.Fatalf("expected mount target wait timeout, got %v", err)
	}
	if fixture.deleted["fs-11111111"] || !fixture.targets["fs-11111111"] {
		t.Fatal("file system deleted before mount target disappeared")
	}
}

func TestEFSRefusesUnsafeCleanupWithoutMutations(t *testing.T) {
	for _, scenario := range []string{"unselected-peer", "protected-peer", "remote-region", "remote-account", "protected-access-point", "permission-denied"} {
		t.Run(scenario, func(t *testing.T) {
			selector := Selector{IDs: []string{"fs-11111111", "fs-22222222"}}
			if scenario == "unselected-peer" {
				selector.IDs = selector.IDs[:1]
			}
			fixture, service := newEFSFixture(t, selector)
			fixture.replicated = true
			wantError := "must also be explicitly selected"
			switch scenario {
			case "protected-peer":
				fixture.protectedPeer = true
				wantError = "DoNotDelete=true"
			case "remote-region":
				fixture.remoteRegion = "us-east-1"
				wantError = "cross-region"
			case "remote-account":
				fixture.remoteOwner = "999999999999"
				wantError = "cross-account"
			case "protected-access-point":
				fixture.protectedAP = true
				wantError = "DoNotDelete=true"
			case "permission-denied":
				fixture.denyMountRead = true
				wantError = "AccessDeniedException"
			}
			err := service.Delete(context.Background(), Resource{ID: "fs-11111111"})
			if err == nil || !strings.Contains(err.Error(), wantError) || fixture.mutations != 0 {
				t.Fatalf("expected refusal without mutation; err=%v, mutations=%d", err, fixture.mutations)
			}
		})
	}
}
