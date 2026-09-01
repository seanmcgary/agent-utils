package ghub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-github/v77/github"
)

// authenticatedUserHandler answers Users.Get(ctx, "") -- GET /user -- with
// login, the identity LatestReviewActivity must exclude its own activity by.
func authenticatedUserHandler(login string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&github.User{Login: github.Ptr(login)})
	}
}

func reviewComment(login, assoc, createdAt string) *github.PullRequestComment {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		panic(err)
	}
	return &github.PullRequestComment{
		User:              &github.User{Login: github.Ptr(login)},
		AuthorAssociation: github.Ptr(assoc),
		CreatedAt:         &github.Timestamp{Time: t},
	}
}

func review(login, assoc, submittedAt string) *github.PullRequestReview {
	r := &github.PullRequestReview{
		User:              &github.User{Login: github.Ptr(login)},
		AuthorAssociation: github.Ptr(assoc),
	}
	if submittedAt != "" {
		t, err := time.Parse(time.RFC3339, submittedAt)
		if err != nil {
			panic(err)
		}
		r.SubmittedAt = &github.Timestamp{Time: t}
	}
	return r
}

// AuthenticatedLogin is a thin wrapper over Users.Get(ctx, ""), memoised. This
// proves both the wrapping and the memoisation.
func TestAuthenticatedLoginIsMemoised(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&github.User{Login: github.Ptr("loop-bot")})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gh := newTestClient(t, srv)

	for i := 0; i < 3; i++ {
		login, err := gh.AuthenticatedLogin(context.Background())
		if err != nil {
			t.Fatalf("AuthenticatedLogin: %v", err)
		}
		if login != "loop-bot" {
			t.Fatalf("AuthenticatedLogin = %q, want loop-bot", login)
		}
	}
	if calls != 1 {
		t.Errorf("Users.Get was called %d times, want 1: the token does not change while the process runs", calls)
	}
}

// LatestReviewActivity applies both filters, reads both endpoints, and caps
// the review walk. Every case shares one fixture shape: comments and reviews
// on pull request 108, answered from a stubbed transport.
func TestLatestReviewActivity(t *testing.T) {
	for _, c := range []struct {
		name     string
		comments []*github.PullRequestComment
		reviews  []*github.PullRequestReview
		want     time.Time
	}{
		{
			name:     "comments only",
			comments: []*github.PullRequestComment{reviewComment("alice", "MEMBER", "2026-01-01T10:00:00Z")},
			want:     time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		},
		{
			name:    "reviews only",
			reviews: []*github.PullRequestReview{review("alice", "MEMBER", "2026-01-02T10:00:00Z")},
			want:    time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC),
		},
		{
			name:     "both, the later one wins",
			comments: []*github.PullRequestComment{reviewComment("alice", "MEMBER", "2026-01-01T10:00:00Z")},
			reviews:  []*github.PullRequestReview{review("alice", "MEMBER", "2026-01-03T10:00:00Z")},
			want:     time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC),
		},
		{
			name:     "both, the comment is later",
			comments: []*github.PullRequestComment{reviewComment("alice", "MEMBER", "2026-01-05T10:00:00Z")},
			reviews:  []*github.PullRequestReview{review("alice", "MEMBER", "2026-01-03T10:00:00Z")},
			want:     time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC),
		},
		{
			name: "neither: the zero time",
			want: time.Time{},
		},
		{
			name:     "a review with no submitted_at is ignored",
			reviews:  []*github.PullRequestReview{review("alice", "MEMBER", "")},
			comments: []*github.PullRequestComment{reviewComment("alice", "MEMBER", "2026-01-01T10:00:00Z")},
			want:     time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		},
		{
			name:     "activity by the authenticated login is ignored",
			comments: []*github.PullRequestComment{reviewComment("loop-bot", "MEMBER", "2026-01-01T10:00:00Z")},
			reviews:  []*github.PullRequestReview{review("loop-bot", "MEMBER", "2026-01-02T10:00:00Z")},
			want:     time.Time{},
		},
		{
			name:     "a CONTRIBUTOR author is ignored",
			comments: []*github.PullRequestComment{reviewComment("mallory", "CONTRIBUTOR", "2026-01-01T10:00:00Z")},
			want:     time.Time{},
		},
		{
			name:    "a NONE author is ignored",
			reviews: []*github.PullRequestReview{review("mallory", "NONE", "2026-01-01T10:00:00Z")},
			want:    time.Time{},
		},
		{
			name:     "an OWNER author counts",
			comments: []*github.PullRequestComment{reviewComment("owner", "OWNER", "2026-01-01T10:00:00Z")},
			want:     time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		},
		{
			name:     "a COLLABORATOR author counts",
			comments: []*github.PullRequestComment{reviewComment("collab", "COLLABORATOR", "2026-01-01T10:00:00Z")},
			want:     time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/user", authenticatedUserHandler("loop-bot"))
			mux.HandleFunc("/repos/o/r/pulls/108/comments", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(c.comments)
			})
			mux.HandleFunc("/repos/o/r/pulls/108/reviews", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(c.reviews)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			gh := newTestClient(t, srv)

			got, err := gh.LatestReviewActivity(context.Background(), "o", "r", 108)
			if err != nil {
				t.Fatalf("LatestReviewActivity: %v", err)
			}
			if !got.Equal(c.want) {
				t.Errorf("LatestReviewActivity = %v, want %v", got, c.want)
			}
		})
	}
}

// The review walk stops at maxReviewPages rather than exhausting the daemon's
// rate limit against a pull request with an unbounded number of reviews.
func TestLatestReviewActivityCapsTheReviewWalk(t *testing.T) {
	pages := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/user", authenticatedUserHandler("loop-bot"))
	mux.HandleFunc("/repos/o/r/pulls/108/comments", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]*github.PullRequestComment{})
	})
	mux.HandleFunc("/repos/o/r/pulls/108/reviews", func(w http.ResponseWriter, r *http.Request) {
		pages++
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		// Every page carries one review, timestamped later than the last, so
		// the walk would keep finding a newer "latest" forever if it were
		// unbounded. Page 12 -- well past the cap -- must never be reached.
		if page == "12" {
			t.Fatal("the review walk must stop at maxReviewPages")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", `<https://x/repos/o/r/pulls/108/reviews?page=`+nextPage(page)+`>; rel="next"`)
		_ = json.NewEncoder(w).Encode([]*github.PullRequestReview{
			review("alice", "MEMBER", "2026-01-01T00:00:00Z"),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gh := newTestClient(t, srv)

	if _, err := gh.LatestReviewActivity(context.Background(), "o", "r", 108); err != nil {
		t.Fatalf("LatestReviewActivity: %v", err)
	}
	if pages != maxReviewPages {
		t.Errorf("reviews endpoint was hit %d times, want the cap of %d", pages, maxReviewPages)
	}
}

func nextPage(page string) string {
	n, err := strconv.Atoi(page)
	if err != nil {
		n = 1
	}
	return strconv.Itoa(n + 1)
}

// The self-filter folds case, and this is the fixture that proves it.
//
// A GitHub login is case-insensitive, and the two sides reach the comparison by
// different routes: self from Users.Get, author from the review payload. With a
// case-sensitive compare the loop stops recognising its own comments the moment
// the two spellings differ. A test using one spelling for both passes either
// way, so the spellings here differ deliberately.
func TestCountsAsReviewActivityFoldsTheSelfLoginCase(t *testing.T) {
	if countsAsReviewActivity("loop-bot", "Loop-Bot", "OWNER") {
		t.Error("activity by the loop's own login in different case counted as somebody else's")
	}
	if !countsAsReviewActivity("loop-bot", "a-reviewer", "OWNER") {
		t.Error("activity by a different OWNER did not count")
	}
}

// An empty login is an error, not an answer. countsAsReviewActivity compares an
// author against this value, and "" matches no author, so returning it would
// silently disable the self-filter while every caller believed it had one.
func TestAuthenticatedLoginRejectsAnEmptyLogin(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&github.User{Login: github.Ptr("")})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gh := newTestClient(t, srv)

	if _, err := gh.AuthenticatedLogin(context.Background()); err == nil {
		t.Error("AuthenticatedLogin returned no error for an empty login")
	}
}
