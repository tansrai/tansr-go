package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCloudJournalRunnerLostRepliesAndColdQuery(t *testing.T) {
	for _, lose := range []string{"", "claim", "complete"} {
		t.Run(lose, func(t *testing.T) {
			op, conn := testOperation(t), testConnection()
			var record *JournalRecord
			claims, completions, effects := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Idempotency-Key") != "" {
					t.Error("claim permission must never be HTTP-replayed")
				}
				var body map[string]json.RawMessage
				if r.Method == "POST" {
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/journal/claim"):
					claims++
					var attempt string
					_ = json.Unmarshal(body["attemptId"], &attempt)
					disposition := "granted"
					out := map[string]any{"protocol": Protocol}
					if record != nil {
						disposition = "pending"
						if record.Receipt != nil {
							disposition = "completed"
						}
					} else {
						record = &JournalRecord{Operation: op, AttemptID: attempt, ConnectionID: conn.ConnectionID, Status: "pending"}
					}
					out["disposition"], out["record"] = disposition, record
					if disposition == "granted" {
						out["claimToken"] = strings.Repeat("a", 43)
					}
					if lose == "claim" && claims == 1 {
						c, _, _ := w.(http.Hijacker).Hijack()
						_ = c.Close()
						return
					}
					executorResponse(t, w, 200, out)
				case strings.HasSuffix(r.URL.Path, "/journal/complete"):
					completions++
					var token string
					_ = json.Unmarshal(body["claimToken"], &token)
					if token != strings.Repeat("a", 43) {
						t.Error("wrong owner token")
					}
					var receipt Receipt
					_ = json.Unmarshal(body["receipt"], &receipt)
					record.Status, record.Receipt = receipt.Status, &receipt
					if lose == "complete" && completions == 1 {
						c, _, _ := w.(http.Hijacker).Hijack()
						_ = c.Close()
						return
					}
					executorResponse(t, w, 200, map[string]any{"protocol": Protocol, "record": record})
				case strings.HasSuffix(r.URL.Path, "/journal"):
					executorResponse(t, w, 200, map[string]any{"protocol": Protocol, "records": []any{record}, "nextCursor": nil})
				default:
					executorResponse(t, w, 200, map[string]any{"protocol": Protocol, "record": record})
				}
			}))
			defer server.Close()
			client := testClient(t, server.URL)
			journal, err := NewCloudJournal(client, "session", func() (Connection, error) { return conn, nil })
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			runner := testRunner(t, client, journal, func(context.Context, map[string]any) (any, error) { effects++; return testResult(), nil })
			receipt, err := runner.Execute(context.Background(), op)
			if lose == "" && err != nil {
				t.Fatal(err)
			}
			if lose != "" && err == nil {
				t.Fatal("lost response silently succeeded")
			}
			cold, _ := NewCloudJournal(client, "session", func() (Connection, error) { return conn, nil })
			defer cold.Close()
			got, err := cold.Read(context.Background(), op)
			if err != nil || got == nil {
				t.Fatal("original-key cold query", err)
			}
			if lose == "claim" {
				if effects != 0 || completions != 0 || got.Status != "pending" {
					t.Fatal("unknown claim executed or guessed completed")
				}
				if _, err = journal.Claim(context.Background(), op); err == nil || claims != 1 {
					t.Fatal("lost claim auto-retried")
				}
				if _, err = cold.Claim(context.Background(), op); err == nil || effects != 0 {
					t.Fatal("another owner pending executed")
				}
			} else {
				if effects != 1 || got.Status != "completed" || got.Receipt == nil {
					t.Fatal("effect count / completion")
				}
				if err = cold.Complete(context.Background(), op, *got.Receipt); err != nil {
					t.Fatal("cold completed replay", err)
				}
				if lose == "" && !equal(receipt, *got.Receipt) {
					t.Fatal("receipt changed")
				}
				other := *got.Receipt
				other.Status = "unknown"
				other.Result = nil
				other.ErrorCode = nil
				if err = cold.Complete(context.Background(), op, other); err == nil {
					t.Fatal("different receipt accepted")
				}
			}
			page, err := cold.List(context.Background(), "", 4)
			if err != nil || len(page.Records) != 1 {
				t.Fatal("journal page", err)
			}
			wrong := op
			wrong.Scope.EndUserID = "other"
			setDigest(t, &wrong)
			if _, err = cold.Read(context.Background(), wrong); !errors.Is(err, ErrConflict) {
				t.Fatal("wrong source", err)
			}
		})
	}
}

func TestCloudJournalConfirmedReadReleasesOnlySameOperation(t *testing.T) {
	var current Operation
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := Resource{Operation: "tool.invoke", Args: map[string]any{"resultJson": "{\"status\":\"ok\",\"content\":[{\"t\":\"text\",\"text\":\"done\"}]}"}}
		receipt := Receipt{Protocol: Protocol, ExecutorID: "device", ConnectionID: "connection", OperationID: current.OperationID, Digest: current.Digest, Status: "completed", Result: &result}
		executorResponse(t, w, 200, map[string]any{"protocol": Protocol, "record": JournalRecord{Operation: current, AttemptID: "attempt", ConnectionID: "connection", Status: "completed", Receipt: &receipt}})
	}))
	defer server.Close()
	j, _ := NewCloudJournal(testClient(t, server.URL), "session", func() (Connection, error) { return testConnection(), nil })
	defer j.Close()
	for i := 0; i < 130; i++ {
		current = testOperation(t)
		current.OperationID = fmt.Sprintf("op-%03d", i)
		setDigest(t, &current)
		j.claims[current.OperationID] = &cloudClaim{operation: clone(current)}
		if _, err := j.Read(context.Background(), current); err != nil {
			t.Fatal(err)
		}
		if len(j.claims) != 0 {
			t.Fatal("terminal read leaked claim slot", i)
		}
	}
	other := clone(current)
	other.Request.Args["argsJson"] = "{}"
	setDigest(t, &other)
	j.claims[current.OperationID] = &cloudClaim{operation: other}
	if _, err := j.Read(context.Background(), current); !errors.Is(err, ErrConflict) || len(j.claims) != 1 {
		t.Fatal("different operation released claim")
	}
}
