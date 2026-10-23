package contractlock

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestRepositoryLock(t *testing.T) {
	report, err := Check(filepath.Join("..", "..", "contract"), "")
	if err != nil {
		t.Fatal(err)
	}
	if report.Baseline != Baseline || report.ManifestRevision != 17 || report.Families != 12 || report.Operations != 91 {
		t.Fatalf("wrong baseline report: %+v", report)
	}
}

func TestCheckRejectsDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *fixture)
		want   string
	}{
		{"one byte", func(t *testing.T, f *fixture) {
			write(t, filepath.Join(f.dir, f.lock.Files[1].Path), []byte("X"))
		}, "contract baseline drift"},
		{"missing file", func(t *testing.T, f *fixture) {
			if err := os.Remove(filepath.Join(f.dir, f.lock.Files[1].Path)); err != nil {
				t.Fatal(err)
			}
		}, "read agent-session-v1.json"},
		{"reference drift", func(t *testing.T, f *fixture) {
			write(t, filepath.Join(f.dir, "reference.txt"), []byte("changed reference"))
		}, "reference baseline drift (not by itself a protocol break)"},
		{"duplicate path", func(t *testing.T, f *fixture) {
			f.lock.Files[2].Path = f.lock.Files[1].Path
			f.saveLock(t)
		}, "duplicate or self-referential"},
		{"case aliased path", func(t *testing.T, f *fixture) {
			f.lock.Files[2].Path = strings.ToUpper(f.lock.Files[1].Path)
			f.saveLock(t)
		}, "duplicate or self-referential"},
		{"duplicate source", func(t *testing.T, f *fixture) {
			f.lock.Files[2].Source = f.lock.Files[1].Source
			f.saveLock(t)
		}, "duplicate or self-referential"},
		{"self reference", func(t *testing.T, f *fixture) {
			f.lock.Files[1].Path = "LOCK.json"
			f.saveLock(t)
		}, "duplicate or self-referential"},
		{"path traversal", func(t *testing.T, f *fixture) {
			f.lock.Files[1].Path = "../outside.json"
			f.saveLock(t)
		}, "invalid relative lock path"},
		{"source traversal", func(t *testing.T, f *fixture) {
			f.lock.Files[1].Source = "../outside.json"
			f.saveLock(t)
		}, "invalid relative lock path"},
		{"source missing from lock", func(t *testing.T, f *fixture) {
			f.lock.Files = append(f.lock.Files[:1], f.lock.Files[2:]...)
			f.saveLock(t)
		}, "is missing from LOCK.json"},
		{"golden missing from lock", func(t *testing.T, f *fixture) {
			f.lock.Files = append(f.lock.Files[:2], f.lock.Files[3:]...)
			f.saveLock(t)
		}, "golden: source"},
		{"manifest source hash mismatch", func(t *testing.T, f *fixture) {
			f.manifest.Families[0].SHA256 = strings.Repeat("1", 64)
			f.saveManifest(t)
		}, "SHA256 differs between manifest and lock"},
		{"manifest golden hash mismatch", func(t *testing.T, f *fixture) {
			for path := range f.manifest.Families[0].Golden {
				f.manifest.Families[0].Golden[path] = strings.Repeat("1", 64)
			}
			f.saveManifest(t)
		}, "golden: source"},
		{"family source reference role", func(t *testing.T, f *fixture) {
			f.lock.Files[1].Role = "reference"
			f.saveLock(t)
		}, "must have contract role"},
		{"missing family", func(t *testing.T, f *fixture) {
			f.manifest.Families = f.manifest.Families[:10]
			f.saveManifest(t)
		}, "inventory: got 10 families"},
		{"replaced family", func(t *testing.T, f *fixture) {
			f.manifest.Families[0].ID = "unknown-family-v1"
			f.saveManifest(t)
		}, "required manifest family"},
		{"duplicate family", func(t *testing.T, f *fixture) {
			f.manifest.Families[1].ID = f.manifest.Families[0].ID
			f.saveManifest(t)
		}, "duplicate manifest family"},
		{"manifest schema hash", func(t *testing.T, f *fixture) {
			f.manifest.SchemaHash = strings.Repeat("2", 64)
			f.saveManifest(t)
		}, "aggregate schemaHash"},
		{"lock schema hash", func(t *testing.T, f *fixture) {
			f.lock.SchemaHash = strings.Repeat("2", 64)
			f.saveLock(t)
		}, "aggregate schemaHash"},
		{"manifest revision", func(t *testing.T, f *fixture) {
			f.manifest.Revision++
			f.saveManifest(t)
		}, "manifest identity differs"},
		{"lock revision", func(t *testing.T, f *fixture) {
			f.lock.ManifestRevision++
			f.saveLock(t)
		}, "frozen baseline requires 17"},
		{"missing manifest", func(t *testing.T, f *fixture) {
			f.lock.Files = f.lock.Files[1:]
			f.saveLock(t)
		}, "lock must include the api-manifest.json source"},
		{"operation count", func(t *testing.T, f *fixture) {
			f.manifest.Operations = f.manifest.Operations[:80]
			f.saveManifest(t)
		}, "require 12 / 91"},
		{"invalid source SHA", func(t *testing.T, f *fixture) {
			f.lock.SourceCommit = "HEAD"
			f.saveLock(t)
		}, "40-digit Git SHA"},
		{"public family cannot use historical reference pin", func(t *testing.T, f *fixture) {
			f.lock.Files[1].SourceCommit = f.lock.SourceCommit
			f.saveLock(t)
		}, "per-file sourceCommit"},
		{"invalid historical reference SHA", func(t *testing.T, f *fixture) {
			f.lock.Files[len(f.lock.Files)-1].SourceCommit = "HEAD"
			f.saveLock(t)
		}, "per-file sourceCommit"},
		{"invalid file SHA", func(t *testing.T, f *fixture) {
			f.lock.Files[1].SHA256 = "SHA256:abc"
			f.saveLock(t)
		}, "64-digit SHA256"},
		{"invalid role", func(t *testing.T, f *fixture) {
			f.lock.Files[1].Role = "ignored"
			f.saveLock(t)
		}, "unknown role"},
		{"two JSON documents", func(t *testing.T, f *fixture) {
			data, err := os.ReadFile(filepath.Join(f.dir, "LOCK.json"))
			if err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(f.dir, "LOCK.json"), append(data, []byte("{}")...))
		}, "exactly one JSON document"},
		{"directory as file", func(t *testing.T, f *fixture) {
			f.lock.Files[1].Path = ".directory"
			if err := os.Mkdir(filepath.Join(f.dir, ".directory"), 0o755); err != nil {
				t.Fatal(err)
			}
			f.saveLock(t)
		}, "expected a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.mutate(t, f)
			_, err := Check(f.dir, "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v; want error containing %q", err, tt.want)
			}
		})
	}
}

func TestValidPaths(t *testing.T) {
	for _, path := range []string{"", ".", "..", "../a", "a/../b", "a//b", "/a", "C:/a", `a\b`, "a/", "a\x00b", "a\nb", "a\rb"} {
		if validPath(path) {
			t.Errorf("unsafe path accepted: %q", path)
		}
	}
	if !validPath("reference/中文/contract.json") {
		t.Error("valid UTF-8 source path rejected")
	}
}

func TestSourceOnlyMetadataNeverClaimsPrivateBytes(t *testing.T) {
	f := newFixture(t)
	file := f.lock.Files[1]
	f.lock.SourceOnly = []SourceFact{{Source: file.Source, SHA256: file.SHA256}}
	f.lock.Files = append(f.lock.Files[:1], f.lock.Files[2:]...)
	f.saveLock(t)
	r, err := Check(f.dir, "")
	if err != nil || r.SourceOnlyMetadata != 1 || r.SourceOnlyBytesVerified {
		t.Fatal(r, err)
	}
	f.lock.SourceOnly[0].SHA256 = strings.Repeat("a", 64)
	f.saveLock(t)
	if _, err = Check(f.dir, ""); err == nil {
		t.Fatal("source-only hash detached from manifest")
	}
}

func TestHistoricalReferenceRemainsAtOriginalGitBytes(t *testing.T) {
	f := newFixture(t)
	source := t.TempDir()
	for _, file := range f.lock.Files {
		b, e := os.ReadFile(filepath.Join(f.dir, file.Path))
		if e != nil {
			t.Fatal(e)
		}
		write(t, filepath.Join(source, file.Source), b)
	}
	runGit(t, source, "init", "--quiet")
	commit := func() {
		runGit(t, source, "-c", "core.autocrlf=false", "add", "--all")
		runGit(t, source, "-c", "user.name=Contract Test", "-c", "user.email=contract-test@localhost", "-c", "commit.gpgsign=false", "-c", "core.hooksPath="+filepath.Join(source, "no-hooks"), "commit", "--quiet", "-m", "fixture")
	}
	commit()
	original := runGit(t, source, "rev-parse", "HEAD")
	ref := &f.lock.Files[len(f.lock.Files)-1]
	ref.SourceCommit = original
	write(t, filepath.Join(source, ref.Source), []byte("new private implementation deliberately not distributed"))
	commit()
	f.lock.SourceCommit = runGit(t, source, "rev-parse", "HEAD")
	f.saveLock(t)
	if _, err := Check(f.dir, source); err != nil {
		t.Fatal("historical reference falsely required private upgrade", err)
	}
	ref.SourceCommit = f.lock.SourceCommit
	f.saveLock(t)
	if _, err := Check(f.dir, source); err == nil {
		t.Fatal("wrong historical Git pin accepted")
	}
}

func TestSourceCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("optional source checkout verification requires git")
	}
	f := newFixture(t)
	source := t.TempDir()
	for _, file := range f.lock.Files {
		data, err := os.ReadFile(filepath.Join(f.dir, file.Path))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(source, filepath.FromSlash(file.Source)), data)
	}
	runGit(t, source, "init", "--quiet")
	runGit(t, source, "-c", "core.autocrlf=false", "add", "--all")
	runGit(t, source, "-c", "user.name=Contract Test", "-c", "user.email=contract-test@localhost", "-c", "commit.gpgsign=false", "-c", "core.hooksPath="+filepath.Join(source, "no-hooks"), "commit", "--quiet", "-m", "fixture")
	f.lock.SourceCommit = runGit(t, source, "rev-parse", "HEAD")
	f.saveLock(t)
	if _, err := Check(f.dir, source); err != nil {
		t.Fatal(err)
	}

	// HEAD may advance without changing any contract, including another consumer's
	// implementation changes. This is not grounds for regenerating our lock.
	write(t, filepath.Join(source, "unrelated.txt"), []byte("unrelated implementation"))
	runGit(t, source, "-c", "core.autocrlf=false", "add", "--all")
	runGit(t, source, "-c", "user.name=Contract Test", "-c", "user.email=contract-test@localhost", "-c", "commit.gpgsign=false", "-c", "core.hooksPath="+filepath.Join(source, "no-hooks"), "commit", "--quiet", "-m", "unrelated")
	report, err := Check(f.dir, source)
	if err != nil || report.SourceHEAD == f.lock.SourceCommit || report.SourceHEAD == "" {
		t.Fatalf("descendant source HEAD: report=%+v error=%v", report, err)
	}

	// Updating both current source and the copy is insufficient: the bytes must
	// also have existed at sourceCommit. Keep this distinct from current drift.
	ref := &f.lock.Files[len(f.lock.Files)-1]
	original, err := os.ReadFile(filepath.Join(f.dir, ref.Path))
	if err != nil {
		t.Fatal(err)
	}
	replacement := []byte("different reference in both working trees")
	write(t, filepath.Join(f.dir, ref.Path), replacement)
	write(t, filepath.Join(source, ref.Source), replacement)
	ref.SHA256 = digest(replacement)
	f.saveLock(t)
	if _, err := Check(f.dir, source); err == nil || !strings.Contains(err.Error(), "reference baseline drift (not by itself a protocol break): sourceCommit") {
		t.Fatalf("incorrect source commit bytes accepted: %v", err)
	}
	write(t, filepath.Join(f.dir, ref.Path), original)
	write(t, filepath.Join(source, ref.Source), original)
	ref.SHA256 = digest(original)
	f.saveLock(t)

	write(t, filepath.Join(source, f.lock.Files[1].Source), []byte("source drift"))
	if _, err := Check(f.dir, source); err == nil || !strings.Contains(err.Error(), "contract baseline drift: source") {
		t.Fatalf("uncommitted source drift accepted: %v", err)
	}

	f.lock.SourceCommit = strings.Repeat("1", 40)
	f.saveLock(t)
	if _, err := Check(f.dir, source); err == nil || !strings.Contains(err.Error(), "unavailable or not an ancestor") {
		t.Fatalf("unavailable source baseline accepted: %v", err)
	}
}

type fixture struct {
	dir      string
	lock     Lock
	manifest manifest
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir()}
	f.lock = Lock{Format: Format, Baseline: Baseline, SourceRepo: "cpple/tansr", SourceBranch: "main",
		SourceCommit: strings.Repeat("0", 40), ManifestRevision: 17, FamilyCount: 12, OperationCount: 91, Policy: "immutable test baseline"}
	f.lock.Files = []File{{Path: "api-manifest.json", Source: manifestSource, Role: "contract"}}
	f.manifest = manifest{Format: "tansr-api-manifest-v1", Contract: "unified-v1", Revision: 17}
	var familyHashes []string
	for _, id := range requiredFamilies {
		source, golden := "source/"+id+".json", "golden/"+id+".json"
		data := []byte(`{"contract":"` + id + `"}`)
		hash := digest(data)
		f.manifest.Families = append(f.manifest.Families, family{ID: id, Source: source, SHA256: hash, Golden: map[string]string{golden: hash}})
		familyHashes = append(familyHashes, id+":"+hash+"\n")
		for i, src := range []string{source, golden} {
			name := id + ".json"
			if i == 1 {
				name = id + ".golden.json"
			}
			f.lock.Files = append(f.lock.Files, File{Path: name, Source: src, SHA256: hash, Role: "contract"})
			write(t, filepath.Join(f.dir, name), data)
		}
	}
	sort.Strings(familyHashes)
	f.manifest.SchemaHash = digest([]byte(strings.Join(familyHashes, "")))
	f.lock.SchemaHash = f.manifest.SchemaHash
	for range 91 {
		f.manifest.Operations = append(f.manifest.Operations, json.RawMessage(`{"name":"fixture"}`))
	}
	data := []byte("reference implementation, not protocol authority")
	write(t, filepath.Join(f.dir, "reference.txt"), data)
	f.lock.Files = append(f.lock.Files, File{Path: "reference.txt", Source: "source/reference.txt", SHA256: digest(data), Role: "reference"})
	f.saveManifest(t)
	return f
}

func (f *fixture) saveLock(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(f.lock)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.dir, "LOCK.json"), data)
}

func (f *fixture) saveManifest(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.dir, "api-manifest.json"), data)
	f.lock.Files[0].SHA256 = digest(data)
	f.saveLock(t)
}

func write(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := git(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return output
}
