// Package contractlock verifies the SDK2/UAPI development baseline. It only reads
// the lock, its vendored files and, optionally, the source checkout; it never
// refreshes a lock or rewrites a contract to make the check pass.
package contractlock

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	Format   = "tansr-sdk2-uapi-lock-v1"
	Baseline = "pst-serve-02-2026-10-23"

	manifestSource = "packages/server/contract/api-manifest.json"
)

// Lock records exact bytes rather than promising that an independently
// reserialized JSON document has the same contract.
type Lock struct {
	Format           string `json:"format"`
	Baseline         string `json:"baseline"`
	SourceRepo       string `json:"sourceRepo"`
	SourceBranch     string `json:"sourceBranch"`
	SourceCommit     string `json:"sourceCommit"`
	ManifestRevision int    `json:"manifestRevision"`
	SchemaHash       string `json:"schemaHash"`
	OperationCount   int    `json:"operationCount"`
	FamilyCount      int    `json:"familyCount"`
	Policy           string `json:"policy"`
	Files            []File `json:"files"`
	// SourceOnly pins a manifest's private source hash without distributing new private bytes.
	SourceOnly []SourceFact `json:"sourceOnly,omitempty"`
}

type SourceFact struct {
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
}

type File struct {
	Path   string `json:"path"`
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
	Role   string `json:"role"`
	// Historical references retain their already-public bytes; only public contracts advance.
	SourceCommit string `json:"sourceCommit,omitempty"`
}

// Report describes a successfully verified snapshot. SourceHEAD is populated
// only when the caller requested comparison with a source checkout.
type Report struct {
	Baseline                string
	SourceCommit            string
	SourceHEAD              string
	ManifestRevision        int
	SchemaHash              string
	Files                   int
	Families                int
	Operations              int
	SourceOnlyMetadata      int
	SourceOnlyBytesVerified bool
}

type family struct {
	ID     string            `json:"id"`
	Source string            `json:"source"`
	SHA256 string            `json:"sha256"`
	Golden map[string]string `json:"golden"`
}

type manifest struct {
	Format     string            `json:"format"`
	Contract   string            `json:"contract"`
	Revision   int               `json:"revision"`
	SchemaHash string            `json:"schemaHash"`
	Families   []family          `json:"families"`
	Operations []json.RawMessage `json:"operations"`
}

var requiredFamilies = []string{
	"agent-session-v1", "sdk2-ext-v1", "sdk2-archive-recovery-v1",
	"archive-sync-v1", "sdk2-cache-v1", "sdk2-cache-core-v1",
	"terminal-services-v1", "terminal-observation-v1", "terminal-profile-v1",
	"terminal-shell-sandbox-v1", "unified-v1", "persistence-custody-handoff-v1",
}

// Check verifies LOCK.json and every listed file beneath contractDir. When
// sourceDir is nonempty, it also verifies the source bytes and that the recorded
// source commit is an ancestor of its HEAD. Later unrelated commits are allowed;
// changes to frozen files, including uncommitted changes, are not.
func Check(contractDir, sourceDir string) (Report, error) {
	var report Report
	root, err := os.OpenRoot(contractDir)
	if err != nil {
		return report, fmt.Errorf("open contract directory: %w", err)
	}
	defer root.Close()
	raw, err := readFile(root, "LOCK.json")
	if err != nil {
		return report, fmt.Errorf("read LOCK.json: %w", err)
	}
	var lock Lock
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		return report, fmt.Errorf("decode LOCK.json: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return report, errors.New("LOCK.json must contain exactly one JSON document")
	}
	if err := validateLock(lock); err != nil {
		return report, err
	}

	bySource := make(map[string]File, len(lock.Files))
	var problems []error
	var manifestBytes []byte
	for _, file := range lock.Files {
		bySource[file.Source] = file
		data, err := readFile(root, file.Path)
		if err != nil {
			problems = append(problems, fmt.Errorf("read %s: %w", file.Path, err))
			continue
		}
		if got := digest(data); got != file.SHA256 {
			problems = append(problems, fmt.Errorf("%s %s SHA256: got %s, locked %s", driftKind(file), file.Path, got, file.SHA256))
		}
		if file.Source == manifestSource {
			manifestBytes = data
		}
	}
	for _, fact := range lock.SourceOnly {
		bySource[fact.Source] = File{Source: fact.Source, SHA256: fact.SHA256, Role: "contract"}
	}
	if len(manifestBytes) == 0 {
		problems = append(problems, errors.New("lock must include the api-manifest.json source"))
	}
	var m manifest
	if len(manifestBytes) > 0 {
		if err := json.Unmarshal(manifestBytes, &m); err != nil {
			problems = append(problems, fmt.Errorf("decode api-manifest.json: %w", err))
		} else if err := validateManifest(m, lock, bySource); err != nil {
			problems = append(problems, err)
		}
	}
	if err := errors.Join(problems...); err != nil {
		return report, err
	}

	if sourceDir != "" {
		report.SourceHEAD, err = checkSource(sourceDir, lock)
		if err != nil {
			return Report{}, err
		}
	}
	report.Baseline = lock.Baseline
	report.SourceCommit = lock.SourceCommit
	report.ManifestRevision = m.Revision
	report.SchemaHash = m.SchemaHash
	report.Files = len(lock.Files)
	report.SourceOnlyMetadata = len(lock.SourceOnly)
	report.SourceOnlyBytesVerified = sourceDir != "" && len(lock.SourceOnly) > 0
	report.Families = len(m.Families)
	report.Operations = len(m.Operations)
	return report, nil
}

func validateLock(lock Lock) error {
	if lock.Format != Format || lock.Baseline != Baseline || lock.SourceRepo != "cpple/tansr" {
		return fmt.Errorf("unsupported lock identity: format=%q baseline=%q sourceRepo=%q", lock.Format, lock.Baseline, lock.SourceRepo)
	}
	if !isHex(lock.SourceCommit, 40) {
		return errors.New("sourceCommit must be a lowercase 40-digit Git SHA")
	}
	if lock.ManifestRevision != 17 {
		return fmt.Errorf("manifestRevision: got %d, frozen baseline requires 17", lock.ManifestRevision)
	}
	if lock.SourceBranch != "main" || !isHex(lock.SchemaHash, 64) || lock.OperationCount != 91 || lock.FamilyCount != 12 || strings.TrimSpace(lock.Policy) == "" {
		return errors.New("lock metadata requires sourceBranch main, a schemaHash, 91 operations, 12 families and a nonempty policy")
	}
	if len(lock.Files) == 0 {
		return errors.New("lock files must not be empty")
	}
	paths, sources := map[string]bool{}, map[string]bool{}
	for _, file := range lock.Files {
		if !validPath(file.Path) || !validPath(file.Source) {
			return fmt.Errorf("invalid relative lock path: path=%q source=%q", file.Path, file.Source)
		}
		// Reject case aliases on every host, rather than accepting a lock on Linux
		// that resolves two entries to one file on Windows.
		p, s := strings.ToLower(file.Path), strings.ToLower(file.Source)
		if p == "lock.json" || paths[p] || sources[s] {
			return fmt.Errorf("duplicate or self-referential lock path: path=%q source=%q", file.Path, file.Source)
		}
		paths[p], sources[s] = true, true
		if !isHex(file.SHA256, 64) {
			return fmt.Errorf("%s must have a lowercase 64-digit SHA256", file.Path)
		}
		if file.Role != "contract" && file.Role != "semantics" && file.Role != "reference" {
			return fmt.Errorf("%s: unknown role %q", file.Path, file.Role)
		}
		if file.SourceCommit != "" && (file.Role == "contract" && !strings.HasPrefix(file.Path, "reference/") || !isHex(file.SourceCommit, 40)) {
			return fmt.Errorf("%s: per-file sourceCommit is only a 40-digit historical reference pin", file.Path)
		}
	}
	for _, fact := range lock.SourceOnly {
		if !validPath(fact.Source) || !isHex(fact.SHA256, 64) {
			return errors.New("invalid source-only contract fact")
		}
		for _, file := range lock.Files {
			if strings.EqualFold(file.Source, fact.Source) && file.SourceCommit == "" {
				return errors.New("source-only fact duplicates current vendored source")
			}
		}
		key := "source-only:" + strings.ToLower(fact.Source)
		if sources[key] {
			return errors.New("duplicate source-only fact")
		}
		sources[key] = true
	}
	return nil
}

func validateManifest(m manifest, lock Lock, bySource map[string]File) error {
	if m.Format != "tansr-api-manifest-v1" || m.Contract != "unified-v1" || m.Revision != lock.ManifestRevision {
		return fmt.Errorf("manifest identity differs from frozen unified-v1 revision %d", lock.ManifestRevision)
	}
	if len(m.Families) != lock.FamilyCount || len(m.Operations) != lock.OperationCount {
		return fmt.Errorf("manifest inventory: got %d families / %d operations, require 12 / 91", len(m.Families), len(m.Operations))
	}
	seen := make(map[string]bool, len(m.Families))
	var hashes []string
	for _, family := range m.Families {
		if seen[family.ID] {
			return fmt.Errorf("duplicate manifest family %q", family.ID)
		}
		seen[family.ID] = true
		if err := requireSource(bySource, family.Source, family.SHA256, true); err != nil {
			return fmt.Errorf("family %s: %w", family.ID, err)
		}
		// Map iteration order is not stable; sorting also keeps error output stable.
		goldens := make([]string, 0, len(family.Golden))
		for source := range family.Golden {
			goldens = append(goldens, source)
		}
		sort.Strings(goldens)
		for _, source := range goldens {
			if err := requireSource(bySource, source, family.Golden[source], false); err != nil {
				return fmt.Errorf("family %s golden: %w", family.ID, err)
			}
		}
		hashes = append(hashes, family.ID+":"+family.SHA256+"\n")
	}
	for _, name := range requiredFamilies {
		if !seen[name] {
			return fmt.Errorf("required manifest family %q is missing", name)
		}
	}
	sort.Strings(hashes)
	if got := digest([]byte(strings.Join(hashes, ""))); got != m.SchemaHash || got != lock.SchemaHash {
		return fmt.Errorf("aggregate schemaHash: got %s, manifest %s, lock %s", got, m.SchemaHash, lock.SchemaHash)
	}
	return nil
}

func requireSource(files map[string]File, source, hash string, contractOnly bool) error {
	file, ok := files[source]
	if !ok {
		return fmt.Errorf("source %q is missing from LOCK.json", source)
	}
	if contractOnly && file.Role != "contract" {
		return fmt.Errorf("source %q must have contract role", source)
	}
	if contractOnly && file.SourceCommit != "" {
		return fmt.Errorf("current family source %q cannot use a historical reference pin", source)
	}
	if file.SHA256 != hash {
		return fmt.Errorf("source %q SHA256 differs between manifest and lock", source)
	}
	return nil
}

func checkSource(sourceDir string, lock Lock) (string, error) {
	head, err := git(sourceDir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read source HEAD: %w", err)
	}
	if _, err := git(sourceDir, "merge-base", "--is-ancestor", lock.SourceCommit, "HEAD"); err != nil {
		return "", fmt.Errorf("locked source commit %s is unavailable or not an ancestor of HEAD: %w", lock.SourceCommit, err)
	}
	if err := checkCommittedFiles(sourceDir, lock); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(sourceDir)
	if err != nil {
		return "", fmt.Errorf("open source directory: %w", err)
	}
	defer root.Close()
	var problems []error
	for _, file := range lock.Files {
		if file.SourceCommit != "" {
			if _, err := git(sourceDir, "merge-base", "--is-ancestor", file.SourceCommit, lock.SourceCommit); err != nil {
				problems = append(problems, fmt.Errorf("historical reference %s commit is not an ancestor: %w", file.Path, err))
			}
			continue // Original blob was checked; references deliberately do not follow current private sources.
		}
		data, err := readFile(root, file.Source)
		if err != nil {
			problems = append(problems, fmt.Errorf("source %s: %w", file.Source, err))
			continue
		}
		if got := digest(data); got != file.SHA256 {
			problems = append(problems, fmt.Errorf("%s source %s SHA256: got %s, locked %s", driftKind(file), file.Source, got, file.SHA256))
		}
	}
	for _, fact := range lock.SourceOnly {
		data, e := readFile(root, fact.Source)
		if e != nil || digest(data) != fact.SHA256 {
			problems = append(problems, fmt.Errorf("source-only current contract drift: %s: %v", fact.Source, e))
		}
	}
	return head, errors.Join(problems...)
}

// One cat-file process checks original commit bytes, independently of today's
// working tree. A copied or accidentally edited sourceCommit must not pass just
// because the current files happen to match the consumer's lock.
func checkCommittedFiles(sourceDir string, lock Lock) error {
	var input strings.Builder
	files := append([]File(nil), lock.Files...)
	for _, fact := range lock.SourceOnly {
		files = append(files, File{Source: fact.Source, SHA256: fact.SHA256, Role: "contract"})
	}
	for _, file := range files {
		commit := lock.SourceCommit
		if file.SourceCommit != "" {
			commit = file.SourceCommit
		}
		fmt.Fprintf(&input, "%s:%s\n", commit, file.Source)
	}
	command := exec.Command("git", "-C", sourceDir, "cat-file", "--batch")
	command.Stdin = strings.NewReader(input.String())
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("read locked source commit: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	buffer := bytes.NewBuffer(output)
	var problems []error
	for _, file := range files {
		commit := lock.SourceCommit
		if file.SourceCommit != "" {
			commit = file.SourceCommit
		}
		header, err := buffer.ReadString('\n')
		parts := strings.Fields(header)
		if err != nil || len(parts) != 3 || parts[1] != "blob" {
			return fmt.Errorf("locked source commit %s does not contain regular blob %q", commit, file.Source)
		}
		size, err := strconv.Atoi(parts[2])
		if err != nil || size < 0 || size >= buffer.Len() {
			return fmt.Errorf("invalid Git blob size for %q", file.Source)
		}
		data := buffer.Next(size)
		terminator, err := buffer.ReadByte()
		if err != nil || terminator != '\n' {
			return fmt.Errorf("incomplete Git blob for %q", file.Source)
		}
		if got := digest(data); got != file.SHA256 {
			problems = append(problems, fmt.Errorf("%s sourceCommit %s file %s SHA256: got %s, locked %s", driftKind(file), commit, file.Source, got, file.SHA256))
		}
	}
	if buffer.Len() != 0 {
		return errors.New("unexpected trailing Git blob output")
	}
	return errors.Join(problems...)
}

func git(dir string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", args[0], err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func readFile(root *os.Root, name string) ([]byte, error) {
	file, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("expected a regular file")
	}
	return io.ReadAll(file)
}

func validPath(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.HasPrefix(name, "../") &&
		!strings.ContainsAny(name, "\\:") && !path.IsAbs(name) && path.Clean(name) == name &&
		strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) == -1
}

func isHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func digest(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func driftKind(file File) string {
	if file.Role == "reference" {
		return "reference baseline drift (not by itself a protocol break):"
	}
	return file.Role + " baseline drift:"
}
