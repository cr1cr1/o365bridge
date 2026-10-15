package owa

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func assertTaskSplitUIDRepair(t *testing.T, req map[string]any, id string) string {
	t.Helper()
	changes := arr(req, "Body", "ItemChanges")
	if len(changes) != 1 || str(changes[0], "ItemId", "Id") != id {
		t.Fatalf("UID repair targets wrong native task: %+v", req)
	}
	updates := arr(changes[0], "Updates")
	if len(updates) != 1 || str(updates[0], "__type") != "SetItemField:#Exchange" {
		t.Fatalf("repair must set exactly one bridge property: %+v", updates)
	}
	update := updates[0]
	path := dig(update, "Path")
	if str(path, "__type") != "ExtendedPropertyUri:#Exchange" || str(path, "DistinguishedPropertySetId") != "PublicStrings" || str(path, "PropertyName") != taskUIDPropertyName || str(path, "PropertyType") != "String" {
		t.Fatalf("repair must target only the bridge UID: %+v", path)
	}
	item := asMap(dig(update, "Item"))
	properties := arr(item, "ExtendedProperty")
	if len(item) != 2 || str(item, "__type") != "Task:#Exchange" || len(properties) != 1 || str(properties[0], "Value") != id || str(properties[0], "ExtendedFieldURI", "DistinguishedPropertySetId") != "PublicStrings" || str(properties[0], "ExtendedFieldURI", "PropertyName") != taskUIDPropertyName || str(properties[0], "ExtendedFieldURI", "PropertyType") != "String" {
		t.Fatalf("repair rewrote native task fields or wrong UID: %+v", item)
	}
	return str(properties[0], "Value")
}

func TestOWATasksUpdateSplitUIDRepair(t *testing.T) {
	for _, mode := range []string{"new native id", "same native id", "no returned id", "initial failure", "repair failure"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			_, p := newFake(t, func(action string, req map[string]any) any {
				if action != "UpdateItem" {
					t.Fatalf("unexpected action %s", action)
				}
				calls++
				if calls == 1 {
					if str(arr(req, "Body", "ItemChanges")[0], "ItemId", "Id") != "original-native" {
						t.Fatalf("wrong initial target: %+v", req)
					}
					switch mode {
					case "same native id":
						return itemResp(map[string]any{"ItemId": itemID("original-native")})
					case "no returned id":
						return itemResp()
					case "initial failure":
						return okBody(map[string]any{"ResponseClass": "Error", "ResponseCode": "ErrorAccessDenied"})
					default:
						return itemResp(map[string]any{"ItemId": itemID("detached-native")})
					}
				}
				if calls != 2 {
					t.Fatalf("repair must not retry or recurse: calls=%d", calls)
				}
				assertTaskSplitUIDRepair(t, req, "detached-native")
				if mode == "repair failure" {
					return okBody(map[string]any{"ResponseClass": "Error", "ResponseCode": "ErrorAccessDenied"})
				}
				return itemResp(map[string]any{"ItemId": itemID("detached-native")})
			})
			due := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
			err := p.UpdateTask(context.Background(), bridge.Task{ID: "original-native", UID: "client-uid", Due: &due})
			wantCalls := 1
			if mode == "new native id" || mode == "repair failure" {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Errorf("UpdateItem calls=%d, want %d", calls, wantCalls)
			}
			wantFailure := mode == "initial failure" || mode == "repair failure"
			if (err != nil) != wantFailure || (wantFailure && !strings.Contains(err.Error(), "ErrorAccessDenied")) {
				t.Errorf("UpdateTask error=%v, want failure=%v", err, wantFailure)
			}
		})
	}
}

type taskSplitRetryLogin struct {
	renewals, logins int
}

func (l *taskSplitRetryLogin) Login(_ context.Context, email string, channel bridge.Channel) (bridge.Session, error) {
	l.logins++
	return bridge.Session{Email: email, Channel: channel, OWAAuthHeader: "Bearer renewed", TokenExpiry: time.Now().Add(time.Hour)}, nil
}

func (l *taskSplitRetryLogin) Renew(_ context.Context, _ string, session bridge.Session) (bridge.Session, error) {
	l.renewals++
	session.OWAAuthHeader = "Bearer renewed"
	session.TokenExpiry = time.Now().Add(time.Hour)
	return session, nil
}

func TestOWATasksSplitUIDRepairAuthDoesNotReplayNativeEdit(t *testing.T) {
	for _, mode := range []string{"repair auth", "initial auth"} {
		t.Run(mode, func(t *testing.T) {
			initialRequests, dateEdits, repairs := 0, 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if action := r.Header.Get("Action"); action != "UpdateItem" {
					t.Errorf("unexpected action %s", action)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				id := str(arr(req, "Body", "ItemChanges")[0], "ItemId", "Id")
				if id == "original-native" {
					initialRequests++
					if mode == "initial auth" && initialRequests == 1 {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					dateEdits++
				} else {
					repairs++
					assertTaskSplitUIDRepair(t, req, "detached-native")
					if mode == "repair auth" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
				}
				_ = json.NewEncoder(w).Encode(itemResp(map[string]any{"ItemId": itemID("detached-native")}))
			}))
			t.Cleanup(srv.Close)
			login := new(taskSplitRetryLogin)
			reg, err := bridge.NewRegistry(bridge.Config{StateDir: t.TempDir(), Login: login, Factory: Factory{BaseURL: srv.URL + "/owa/"}})
			if err != nil {
				t.Fatal(err)
			}
			const email = "user@example.com"
			if err := reg.Store().SaveAccount(bridge.AccountState{Email: email, Session: bridge.Session{Email: email, Channel: bridge.ChannelOWA, OWAAuthHeader: "Bearer initial", TokenExpiry: time.Now().Add(time.Hour)}}); err != nil {
				t.Fatal(err)
			}
			callbacks := 0
			err = reg.Call(context.Background(), email, func(ctx context.Context, p bridge.Provider) error {
				callbacks++
				due := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
				return p.UpdateTask(ctx, bridge.Task{ID: "original-native", UID: "client-uid", Due: &due})
			})
			if mode == "repair auth" {
				if err == nil || bridge.KindOf(err) != bridge.KindOther || bridge.KindOf(errors.Unwrap(err)) != bridge.KindAuth {
					t.Errorf("partial-write failure must be non-replayable but preserve auth cause: %v", err)
				}
				if callbacks != 1 || initialRequests != 1 || dateEdits != 1 || repairs != 1 || login.renewals != 0 || login.logins != 0 {
					t.Errorf("already-applied native edit replayed: callbacks=%d requests=%d dateEdits=%d repairs=%d renewals=%d logins=%d", callbacks, initialRequests, dateEdits, repairs, login.renewals, login.logins)
				}
			} else if err != nil || callbacks != 2 || initialRequests != 2 || dateEdits != 1 || repairs != 1 || login.renewals != 1 || login.logins != 0 {
				t.Errorf("initial auth failure must retain safe renewal: err=%v callbacks=%d requests=%d dateEdits=%d repairs=%d renewals=%d logins=%d", err, callbacks, initialRequests, dateEdits, repairs, login.renewals, login.logins)
			}
		})
	}
}

func TestOWATasksWriteAssignedSplitUIDSurvivesFinalAndDeletion(t *testing.T) {
	const original, detached, clientUID = "original-native", "detached-native", "client-uid"
	stored := map[string]map[string]any{original: taskOccurrenceItem(original, clientUID, "false", true)}
	updates := 0
	_, p := newFake(t, func(action string, req map[string]any) any {
		switch action {
		case "UpdateItem":
			updates++
			switch updates {
			case 1:
				stored[detached] = taskOccurrenceItem(detached, clientUID, "true", false)
				return itemResp(map[string]any{"ItemId": itemID(detached)})
			case 2:
				uid := assertTaskSplitUIDRepair(t, req, detached)
				asMap(arr(stored[detached], "ExtendedProperty")[0])["Value"] = uid
				return itemResp(map[string]any{"ItemId": itemID(detached)})
			case 3:
				if str(arr(req, "Body", "ItemChanges")[0], "ItemId", "Id") != original {
					t.Fatalf("final completion targeted wrong task: %+v", req)
				}
				asMap(arr(stored[original], "ExtendedProperty")[1])["Value"] = "true"
				stored[original]["Status"], stored[original]["PercentComplete"] = "Completed", 100
				return itemResp(map[string]any{"ItemId": itemID(original)})
			default:
				t.Fatalf("unexpected update %d", updates)
			}
		case "FindItem":
			var rows []any
			for _, item := range stored {
				rows = append(rows, map[string]any{"ItemId": item["ItemId"]})
			}
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": rows, "TotalItemsInView": len(rows), "IncludesLastItemInRange": true}})
		case "GetItem":
			var full []any
			for _, id := range arr(req, "Body", "ItemIds") {
				full = append(full, stored[str(id, "Id")])
			}
			return itemResp(full...)
		case "DeleteItem":
			if ids := arr(req, "Body", "ItemIds"); len(ids) != 1 || str(ids[0], "Id") != original {
				t.Fatalf("delete targeted wrong task: %+v", req)
			}
			delete(stored, original)
			return okBody(nil)
		default:
			t.Fatalf("unexpected action %s", action)
		}
		return okBody(nil)
	})
	ctx := context.Background()
	due := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	if err := p.UpdateTask(ctx, bridge.Task{ID: original, UID: clientUID, Due: &due}); err != nil {
		t.Fatal(err)
	}
	if updates != 2 {
		t.Fatalf("native split did not assign durable detached UID: updates=%d", updates)
	}
	check := func(reader bridge.Provider, count int) {
		t.Helper()
		tasks, err := reader.ListTasks(ctx, "tasks")
		if err != nil || len(tasks) != count {
			t.Fatalf("ListTasks=%+v, %v", tasks, err)
		}
		for _, task := range tasks {
			want := clientUID
			if task.ID == detached {
				want = detached
			}
			if task.UID != want {
				t.Fatalf("identity changed: %+v, want %s", task, want)
			}
		}
	}
	check(p, 2)
	if err := p.UpdateTask(ctx, bridge.Task{ID: original, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	check(p, 2)
	if err := p.DeleteTask(ctx, "tasks", original); err != nil {
		t.Fatal(err)
	}
	check(p, 1)
	fresh := Factory{BaseURL: p.(*provider).base}.New("user@example.com", bridge.Session{OWAAuthHeader: "Bearer fake"})
	check(fresh, 1)
}
