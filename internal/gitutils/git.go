package gitutils

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport/http"
)

// logWriter implements io.Writer to redirect git progress to slog.
type logWriter struct{}

func (w *logWriter) Write(p []byte) (n int, err error) {
	// git progress often uses \r or partial lines. We trim whitespace
	// to avoid empty log entries, but we log everything.
	// Using Debug to avoid flooding Info logs with progress percentages if configured.
	// However, user asked for it to be in slog, mimicking previous stdout visibility.
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		slog.Debug(msg)
	}
	return len(p), nil
}

// RepoInfo holds details about a Git repository.
type RepoInfo struct {
	URL           string
	Branch        string
	CommitHash    string
	CommitURL     string
	CommitMessage string
	CommitDate    string // Formatted date string
}

// GetRepoInfo retrieves detailed information about a local Git repository.
func GetRepoInfo(path string, remoteURL string) (*RepoInfo, error) {
	r, err := git.PlainOpen(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open repo at %s: %w", path, err)
	}
	defer func() { _ = r.Close() }()

	headRef, err := r.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD reference: %w", err)
	}

	commit, err := r.CommitObject(headRef.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get commit object: %w", err)
	}

	branchName := ""
	iter, err := r.Branches()
	if err == nil {
		err = iter.ForEach(func(ref *plumbing.Reference) error {
			if ref.Hash() == headRef.Hash() {
				branchName = ref.Name().Short()
				return fmt.Errorf("found") // Hack to exit ForEach
			}

			return nil
		})
		if err != nil && err.Error() != "found" {
			slog.Debug("Failed to find branch for HEAD", "error", err)
		}
	}

	// Try to determine a GitHub commit URL
	commitURL := ""
	if remoteURL != "" && strings.Contains(remoteURL, "github.com") {
		// Assuming GitHub URL format
		repoPath := strings.TrimPrefix(remoteURL, "https://github.com/")
		repoPath = strings.TrimSuffix(repoPath, ".git")
		commitURL = fmt.Sprintf("https://github.com/%s/commit/%s", repoPath, commit.Hash.String())
	}

	return &RepoInfo{
		URL:           remoteURL,
		Branch:        branchName,
		CommitHash:    commit.Hash.String(),
		CommitURL:     commitURL,
		CommitMessage: strings.SplitN(commit.Message, "\n", 2)[0], // First line of message
		CommitDate:    commit.Author.When.Format("2006-01-02 15:04"),
	}, nil
}

// EnsureRepo clones a repository using its default branch, preserving the
// historical behavior for user-managed custom repositories.
func EnsureRepo(path string, repoURL string, token string, update bool) error {
	return ensureRepo(path, repoURL, "", token, update)
}

// EnsureRepoAtRef makes the system-apps checkout deterministic. The ref is a
// branch name and expectedCommit may be either a full SHA or an unambiguous
// prefix. The returned info is suitable for sanitized startup diagnostics.
func EnsureRepoAtRef(path, repoURL, ref, expectedCommit, token string, update bool) (*RepoInfo, error) {
	if err := ensureRepo(path, repoURL, strings.TrimSpace(ref), token, update); err != nil {
		return nil, err
	}
	info, err := GetRepoInfo(path, repoURL)
	if err != nil {
		return nil, err
	}
	if err := verifyResolvedRepo(info, ref, expectedCommit); err != nil {
		return nil, err
	}
	return info, nil
}

func verifyResolvedRepo(info *RepoInfo, ref, expectedCommit string) error {
	if info == nil {
		return errors.New("resolved apps repository info is missing")
	}
	if ref != "" && info.Branch != ref {
		return fmt.Errorf("resolved apps ref %q, expected %q", info.Branch, ref)
	}
	expectedCommit = strings.ToLower(strings.TrimSpace(expectedCommit))
	if expectedCommit != "" && !strings.HasPrefix(strings.ToLower(info.CommitHash), expectedCommit) {
		return fmt.Errorf("resolved apps commit %s, expected %s", info.CommitHash, expectedCommit)
	}
	return nil
}

// ensureRepo clones a repo if it doesn't exist, or pulls if it does and update is true.
func ensureRepo(path string, repoURL string, ref string, token string, update bool) error {
	slog.Info("Checking git repo", "path", path, "url", repoURL, "ref", ref)

	var clientOpts []client.Option

	u, err := url.Parse(repoURL)
	if err == nil && u.User == nil {
		if token != "" && (u.Scheme == "http" || u.Scheme == "https") && u.Host == "github.com" {
			clientOpts = append(clientOpts, client.WithHTTPAuth(&http.BasicAuth{
				Username: token,
				Password: "", // For GitHub PATs, the password can be empty.
			}))
		}
	} else if err != nil {
		slog.Warn("Failed to parse repo URL", "url", repoURL, "error", err)
	}

	// Check if path exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		slog.Info("Cloning repo", "url", repoURL)
		cloneOptions := &git.CloneOptions{
			URL:           repoURL,
			Progress:      &logWriter{},
			Depth:         1,
			SingleBranch:  true,
			Tags:          git.NoTags,
			ClientOptions: clientOpts,
		}
		if ref != "" {
			cloneOptions.ReferenceName = plumbing.NewBranchReferenceName(ref)
		}
		r, err := git.PlainClone(path, cloneOptions)
		if err == nil {
			_ = r.Close()
		}

		return err
	}

	// Repo exists, open it
	r, err := git.PlainOpen(path)
	if err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			slog.Warn("Directory exists but is not a valid git repo, re-cloning", "path", path)
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("failed to remove invalid repo directory: %w", err)
			}
			return ensureRepo(path, repoURL, ref, token, update)
		}
		// If not a git repo, maybe remove and re-clone?
		// For safety, error out.
		return fmt.Errorf("failed to open repo: %w", err)
	}
	defer func() { _ = r.Close() }()

	// Check remote URL
	rem, err := r.Remote("origin")
	if err != nil || len(rem.Config().URLs) == 0 || rem.Config().URLs[0] != repoURL {
		reason := "remote URL mismatch"
		if err != nil {
			reason = fmt.Sprintf("error getting remote: %v", err)
		} else if len(rem.Config().URLs) == 0 {
			reason = "no remote URLs"
		}

		slog.Warn("Repo validation failed, re-cloning", "reason", reason, "new", repoURL)
		// Remove and re-clone
		_ = r.Close()
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("failed to remove old repo: %w", err)
		}

		return ensureRepo(path, repoURL, ref, token, update)
	}

	if ref != "" {
		headRef, err := r.Head()
		if err != nil || !headRef.Name().IsBranch() || headRef.Name().Short() != ref {
			actual := "detached"
			if err == nil && headRef.Name().IsBranch() {
				actual = headRef.Name().Short()
			}
			slog.Warn("Repo ref mismatch, re-cloning", "actual", actual, "expected", ref)
			_ = r.Close()
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("failed to remove checkout with wrong ref: %w", err)
			}
			return ensureRepo(path, repoURL, ref, token, update)
		}
	}

	if !update {
		slog.Info("Skipping repo update (update=false)")
		return nil
	}

	// Pull
	w, err := r.Worktree()
	if err != nil {
		return err
	}

	// Identify the branch we are on
	headRef, err := r.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}

	if !headRef.Name().IsBranch() {
		return fmt.Errorf("repository in detached HEAD state at %s, cannot determine branch to update", headRef.Hash().String())
	}
	branchName := headRef.Name().Short()

	// Fetch updates (Depth 1 = Shallow)
	// We must explicitly specify the RefSpec to ensure we fetch the branch we are currently on
	// into the correct remote tracking branch. This is crucial for single-branch shallow clones.
	slog.Info("Fetching updates", "branch", branchName)
	refSpec := config.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branchName, branchName))

	err = r.Fetch(&git.FetchOptions{
		RemoteName:    "origin",
		Progress:      &logWriter{},
		Depth:         1,
		Tags:          git.NoTags,
		Force:         true,
		ClientOptions: clientOpts,
		RefSpecs:      []config.RefSpec{refSpec},
	})

	// Handle fetch errors
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		// If fetch fails with object not found (or other critical git error), try re-cloning
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			slog.Warn("Git fetch failed with object not found, re-cloning", "error", err)
			_ = r.Close()
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("failed to remove broken repo: %w", err)
			}
			return ensureRepo(path, repoURL, ref, token, update)
		}
		return fmt.Errorf("failed to fetch repo: %w", err)
	}

	// Find the commit hash of that branch on the remote
	// We look for refs/remotes/origin/<branchName>
	remoteRefName := plumbing.ReferenceName(fmt.Sprintf("refs/remotes/origin/%s", branchName))
	remoteRef, err := r.Reference(remoteRefName, true)
	if err != nil {
		slog.Warn("Failed to find remote ref, re-cloning", "ref", remoteRefName, "error", err)
		_ = r.Close()
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("failed to remove broken repo: %w", err)
		}
		return ensureRepo(path, repoURL, ref, token, update)
	}

	// Hard Reset the worktree to the remote commit
	slog.Info("Resetting to remote HEAD", "commit", remoteRef.Hash().String())
	if err := w.Reset(&git.ResetOptions{
		Mode:   git.HardReset,
		Commit: remoteRef.Hash(),
	}); err != nil {
		return fmt.Errorf("failed to reset worktree: %w", err)
	}

	// Clean untracked files
	if err := w.Clean(&git.CleanOptions{Dir: true}); err != nil {
		slog.Warn("Failed to clean repo", "error", err)
	}

	return nil
}
