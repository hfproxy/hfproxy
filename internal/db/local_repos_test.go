package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func localTestRepo(t *testing.T, store *Store, name string) (repoID, owner string) {
	t.Helper()
	ctx := context.Background()
	owner = NewID()
	if err := store.CreateUser(ctx, owner, "pub-"+name, "", "", "h", "reader"); err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateLocalRepository(ctx, "models", "pub-"+name+"/model", "public", owner)
	if err != nil {
		t.Fatal(err)
	}
	return id, owner
}

func localTestBlob(t *testing.T, store *Store, content string, keep time.Duration) (sum string, size int64) {
	t.Helper()
	sum = strings.Repeat("a", 63-len(content)) + "0" + content // fake but unique 64 hex-ish value
	sum = sum[:64]
	if err := store.RegisterUploadedBlob(context.Background(), sum, "blobs/"+sum, "text/plain", int64(len(content)), keep); err != nil {
		t.Fatal(err)
	}
	return sum, int64(len(content))
}

func TestLocalRepositoryHistoryAndTree(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	repo, owner := localTestRepo(t, store, "tree")

	head, err := store.ResolveLocalRevision(ctx, repo, "main")
	if err != nil || len(head) != 40 {
		t.Fatalf("a new repository has a first commit on main: %q %v", head, err)
	}
	if files, _ := store.LocalFilesAt(ctx, repo, head); len(files) != 0 {
		t.Fatalf("the first commit holds %d files", len(files))
	}

	a, aSize := localTestBlob(t, store, "alpha", time.Hour)
	b, bSize := localTestBlob(t, store, "bravo", time.Hour)
	first, err := store.CommitLocal(ctx, CommitInput{RepositoryID: repo, Branch: "main", ParentCommit: head, AuthorID: owner, AuthorName: "pub", Summary: "add two",
		Ops: []CommitOp{
			{Kind: OpAdd, Path: "README.md", SHA256: a, OID: "gitoid-a", Size: aSize, ContentType: "text/markdown"},
			{Kind: OpAdd, Path: "weights/model.bin", SHA256: b, OID: b, Size: bSize, LFS: true},
		}})
	if err != nil || first.Parent != head || len(first.SHA) != 40 {
		t.Fatalf("first commit = %+v %v", first, err)
	}
	files, _ := store.LocalFilesAt(ctx, repo, first.SHA)
	if len(files) != 2 || files[0].Path != "README.md" || files[1].Path != "weights/model.bin" || !files[1].LFS || files[0].LFS || files[0].OID != "gitoid-a" {
		t.Fatalf("tree = %+v", files)
	}

	// The old commit is untouched by a later one; replacing and deleting work.
	c, cSize := localTestBlob(t, store, "charlie", time.Hour)
	second, err := store.CommitLocal(ctx, CommitInput{RepositoryID: repo, Branch: "main", Summary: "change",
		Ops: []CommitOp{
			{Kind: OpAdd, Path: "README.md", SHA256: c, OID: "gitoid-c", Size: cSize},
			{Kind: OpDeleteFolder, Path: "weights"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := store.LocalFilesAt(ctx, repo, second.SHA); len(files) != 1 || files[0].SHA256 != c {
		t.Fatalf("second tree = %+v", files)
	}
	if files, _ := store.LocalFilesAt(ctx, repo, first.SHA); len(files) != 2 {
		t.Fatalf("history was rewritten: %+v", files)
	}
	history, err := store.LocalHistory(ctx, repo, second.SHA, 10)
	if err != nil || len(history) != 3 || history[0].Summary != "change" || history[2].Summary != "initial commit" {
		t.Fatalf("history = %+v %v", history, err)
	}

	// Stale parent, unknown branch, missing content, file/directory clash.
	if _, err := store.CommitLocal(ctx, CommitInput{RepositoryID: repo, Branch: "main", ParentCommit: first.SHA, Summary: "late"}); !errors.Is(err, ErrParentMismatch) {
		t.Fatalf("stale parent = %v", err)
	}
	if _, err := store.CommitLocal(ctx, CommitInput{RepositoryID: repo, Branch: "nope", Summary: "x"}); !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("unknown branch = %v", err)
	}
	var missing *BlobMissingError
	if _, err := store.CommitLocal(ctx, CommitInput{RepositoryID: repo, Branch: "main", Summary: "x", Ops: []CommitOp{{Kind: OpAdd, Path: "x", SHA256: strings.Repeat("f", 64), Size: 1}}}); !errors.As(err, &missing) {
		t.Fatalf("content that was never uploaded = %v", err)
	}
	if _, err := store.CommitLocal(ctx, CommitInput{RepositoryID: repo, Branch: "main", Summary: "x", Ops: []CommitOp{{Kind: OpAdd, Path: "README.md/inner", SHA256: c, Size: cSize}}}); !errors.Is(err, ErrPathConflict) {
		t.Fatalf("a file used as a directory = %v", err)
	}
	if got, _ := store.ResolveLocalRevision(ctx, repo, "main"); got != second.SHA {
		t.Fatal("a failed commit moved the branch")
	}

	// References.
	if err := store.CreateLocalRef(ctx, repo, "tag", "v1", first.SHA); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLocalRef(ctx, repo, "tag", "v1", first.SHA); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate tag = %v", err)
	}
	if err := store.CreateLocalRef(ctx, repo, "branch", "dev", "main"); err != nil {
		t.Fatal(err)
	}
	for revision, want := range map[string]string{"v1": first.SHA, "refs/tags/v1": first.SHA, "dev": second.SHA, "refs/heads/main": second.SHA, first.SHA: first.SHA} {
		if got, err := store.ResolveLocalRevision(ctx, repo, revision); err != nil || got != want {
			t.Errorf("%s resolves to %q %v, want %q", revision, got, err, want)
		}
	}
	if _, err := store.ResolveLocalRevision(ctx, repo, "nothing"); !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("unknown revision = %v", err)
	}
	if err := store.DeleteLocalRef(ctx, repo, "branch", "main"); err == nil {
		t.Fatal("main must not be deletable")
	}
	if err := store.DeleteLocalRef(ctx, repo, "tag", "v1"); err != nil {
		t.Fatal(err)
	}

	// Bytes owned count each distinct blob once.
	if bytes, err := store.OwnerLocalBytes(ctx, owner); err != nil || bytes != aSize+bSize+cSize {
		t.Fatalf("owner bytes = %d %v, want %d", bytes, err, aSize+bSize+cSize)
	}
}

func TestPublishedContentIsKeptUntilItsCommitAndNeverEvicted(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	repo, _ := localTestRepo(t, store, "keep")
	sum, size := localTestBlob(t, store, "kept", 23*time.Hour)

	// Uploaded but not committed: not collectable yet.
	ids, err := store.OrphanedBlobIDs(ctx, time.Hour)
	if err != nil || len(ids) != 0 {
		t.Fatalf("an uploaded blob was offered to the collector: %v %v", ids, err)
	}
	if _, err := store.CommitLocal(ctx, CommitInput{RepositoryID: repo, Branch: "main", Summary: "add", Ops: []CommitOp{{Kind: OpAdd, Path: "f", SHA256: sum, Size: size}}}); err != nil {
		t.Fatal(err)
	}
	var orphaned *time.Time
	if err := store.SQL.QueryRow(`SELECT orphaned_at FROM blobs WHERE sha256=$1`, sum).Scan(&orphaned); err != nil || orphaned != nil {
		t.Fatalf("a committed blob must not stay marked orphaned: %v %v", orphaned, err)
	}
	if candidates, err := store.EvictionCandidates(ctx, EvictionPolicy{}, EvictionScope{}, 100); err != nil || len(candidates) != 0 {
		t.Fatalf("published files are eviction candidates: %v %v", candidates, err)
	}
	if live, err := store.LiveBytes(ctx); err != nil || live != 0 {
		t.Fatalf("published content counts %d bytes against the cache limit (%v)", live, err)
	}
	stats, err := store.CacheStats(ctx, "admin", "x")
	if err != nil || stats.Artifacts != 0 {
		t.Fatalf("published files are counted as cached files: %+v %v", stats, err)
	}
	artifactID := ""
	_ = store.SQL.QueryRow(`SELECT id FROM artifacts WHERE repository_id=$1`, repo).Scan(&artifactID)
	if err := store.PurgeArtifact(ctx, artifactID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the cache purge removed a published file: %v", err)
	}
}

func TestDeletingAPublishedRepositoryOrphansItsContentOnly(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	repoA, _ := localTestRepo(t, store, "dela")
	repoB, _ := localTestRepo(t, store, "delb")
	shared, sharedSize := localTestBlob(t, store, "shared", time.Hour)
	only, onlySize := localTestBlob(t, store, "only-a", time.Hour)
	for _, c := range []struct {
		repo string
		ops  []CommitOp
	}{
		{repoA, []CommitOp{{Kind: OpAdd, Path: "s", SHA256: shared, Size: sharedSize}, {Kind: OpAdd, Path: "o", SHA256: only, Size: onlySize}}},
		{repoB, []CommitOp{{Kind: OpAdd, Path: "s", SHA256: shared, Size: sharedSize}}},
	} {
		if _, err := store.CommitLocal(ctx, CommitInput{RepositoryID: c.repo, Branch: "main", Summary: "add", Ops: c.ops}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DeleteLocalRepository(ctx, repoA); err != nil {
		t.Fatal(err)
	}
	orphaned := func(sum string) bool {
		var at *time.Time
		if err := store.SQL.QueryRow(`SELECT orphaned_at FROM blobs WHERE sha256=$1`, sum).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at != nil
	}
	if !orphaned(only) || orphaned(shared) {
		t.Fatalf("after deleting A: only-a orphaned=%v (want true), shared orphaned=%v (want false)", orphaned(only), orphaned(shared))
	}
	if err := store.DeleteLocalRepository(ctx, repoA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice = %v", err)
	}
	if _, err := store.ResolveLocalRevision(ctx, repoA, "main"); !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("the history of a deleted repository is still there: %v", err)
	}
	// The name is free again, and a Hub repository cannot take a published name.
	if _, err := store.CreateLocalRepository(ctx, "models", "pub-dela/model", "public", ""); err != nil {
		t.Fatalf("the name was not released: %v", err)
	}
	if _, err := store.UpsertRepository(ctx, "models", "pub-delb/model", "public", ""); !errors.Is(err, ErrExists) {
		t.Fatalf("registering a Hub repository over a published one = %v", err)
	}
	if _, err := store.CreateLocalRepository(ctx, "models", "pub-delb/model", "public", ""); !errors.Is(err, ErrExists) {
		t.Fatalf("creating a published repository twice = %v", err)
	}
}

func TestUsersAndGroupsDoNotShareNames(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	user := NewID()
	if err := store.CreateUser(ctx, user, "acme", "", "", "h", "reader"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateGroup(ctx, NewID(), "acme", nil); !errors.Is(err, ErrExists) {
		t.Fatalf("a group named like a user = %v, want ErrExists", err)
	}
	group := NewID()
	if err := store.CreateGroup(ctx, group, "research", []string{user}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateUser(ctx, NewID(), "research", "", "", "h", "reader"); !errors.Is(err, ErrExists) {
		t.Fatalf("a user named like a group = %v, want ErrExists", err)
	}
	if n, err := store.ResolveNamespace(ctx, "acme"); err != nil || n.Kind != "user" || n.ID != user {
		t.Fatalf("acme = %+v %v", n, err)
	}
	if n, err := store.ResolveNamespace(ctx, "research"); err != nil || n.Kind != "group" || n.ID != group {
		t.Fatalf("research = %+v %v", n, err)
	}
	if _, err := store.ResolveNamespace(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown namespace = %v", err)
	}
	if member, err := store.IsGroupMember(ctx, group, user); err != nil || !member {
		t.Fatalf("membership = %v %v", member, err)
	}
	if names, _ := store.UserGroupNames(ctx, user); len(names) != 1 || names[0] != "research" {
		t.Fatalf("groups = %v", names)
	}
}

// Many small commits must read back exactly as a full copy of the tree would,
// while storing only what each commit changed.
func TestLongHistoriesAreStoredAsDeltasAndReadBackExactly(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	repo, _ := localTestRepo(t, store, "delta")

	// A fixed pool of contents; commits add, replace and remove files among them.
	contents := make([]struct {
		sum  string
		size int64
	}, 6)
	for i := range contents {
		contents[i].sum, contents[i].size = localTestBlob(t, store, strings.Repeat(string(rune('a'+i)), i+1), time.Hour)
	}
	model := map[string]string{} // path -> content sum, the tree after the last commit
	var history []struct {
		sha  string
		tree map[string]string
	}
	copyTree := func() map[string]string {
		out := make(map[string]string, len(model))
		for k, v := range model {
			out[k] = v
		}
		return out
	}
	step := 0
	next := func(n int) int { step = (step*7 + 13 + n) % 1009; return step }
	const commits = 100
	for c := 0; c < commits; c++ {
		var ops []CommitOp
		for k := 0; k < 3; k++ {
			path := "dir" + string(rune('0'+next(k)%3)) + "/file" + string(rune('a'+next(k+3)%8))
			switch next(k+5) % 4 {
			case 0: // remove
				ops = append(ops, CommitOp{Kind: OpDeleteFile, Path: path})
				delete(model, path)
			case 1: // remove a folder
				folder := "dir" + string(rune('0'+next(k)%3))
				ops = append(ops, CommitOp{Kind: OpDeleteFolder, Path: folder})
				for p := range model {
					if strings.HasPrefix(p, folder+"/") {
						delete(model, p)
					}
				}
			default: // add or replace
				content := contents[next(k+7)%len(contents)]
				ops = append(ops, CommitOp{Kind: OpAdd, Path: path, SHA256: content.sum, OID: content.sum, Size: content.size})
				model[path] = content.sum
			}
		}
		commit, err := store.CommitLocal(ctx, CommitInput{RepositoryID: repo, Branch: "main", Summary: "c", Ops: ops})
		if err != nil {
			t.Fatalf("commit %d: %v", c, err)
		}
		history = append(history, struct {
			sha  string
			tree map[string]string
		}{commit.SHA, copyTree()})
	}

	for i, h := range history {
		files, err := store.LocalFilesAt(ctx, repo, h.sha)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, f := range files {
			got[f.Path] = f.SHA256
		}
		if len(got) != len(h.tree) {
			t.Fatalf("commit %d: %d files, want %d\n got %v\nwant %v", i, len(got), len(h.tree), got, h.tree)
		}
		for path, sum := range h.tree {
			if got[path] != sum {
				t.Fatalf("commit %d: %s = %q, want %q", i, path, got[path], sum)
			}
			single, err := store.LocalFileAt(ctx, repo, h.sha, path)
			if err != nil || single.SHA256 != sum {
				t.Fatalf("commit %d: LocalFileAt(%s) = %+v %v", i, path, single, err)
			}
		}
		if _, err := store.LocalFileAt(ctx, repo, h.sha, "dir9/none"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a file that never existed = %v", err)
		}
	}

	// Storage: far fewer rows than a copy of the tree per commit, and snapshots
	// bound how far a read walks.
	var rows, snapshots, maxDepth int
	store.SQL.QueryRow(`SELECT count(*) FROM artifacts WHERE repository_id=$1`, repo).Scan(&rows)
	store.SQL.QueryRow(`SELECT count(*) FILTER (WHERE snapshot), max(depth) FROM local_commits WHERE repository_id=$1`, repo).Scan(&snapshots, &maxDepth)
	fullCopies := 0
	for _, h := range history {
		fullCopies += len(h.tree)
	}
	if rows >= fullCopies/2 {
		t.Errorf("%d artifact rows for %d commits: the history is not stored as deltas (a copy per commit would be %d)", rows, commits, fullCopies)
	}
	if snapshots < commits/snapshotEvery || maxDepth >= snapshotEvery {
		t.Errorf("snapshots = %d, deepest chain = %d, want a snapshot every %d commits", snapshots, maxDepth, snapshotEvery)
	}
}
