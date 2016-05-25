package pkgmirror

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/boltdb/bolt"
	"github.com/rande/goapp"
)

var (
	BITBUCKET_ARCHIVE = regexp.MustCompile(`http(s|):\/\/([\w-\.]+)\/([\w-]+)\/([\w-]+)\/get\/([\w]+)\.zip`)
	GITHUB_ARCHIVE    = regexp.MustCompile(`http(s|):\/\/api\.([\w-\.]+)\/repos\/([\w-]+)\/([\w-]+)\/zipball\/([\w]+)`)
	GITLAB_ARCHIVE    = regexp.MustCompile(`http(s|):\/\/([\w-\.]+)\/([\w-]+)\/([\w-]+)\/repository\/archive.zip\?ref=([\w]+)`)

	GIT_REPOSITORY = regexp.MustCompile(`^(((git|http(s|)):\/\/|git@))([\w-\.]+@|)([\w-\.]+)(\/|:)([\w-\.\/]+?)(\.git|)$`)
	SVN_REPOSITORY = regexp.MustCompile(`(svn:\/\/(.*)|(.*)\.svn\.(.*))`)
)

func NewGitService() *GitService {
	return &GitService{
		Config: &GitConfig{
			Code:   []byte("git"),
			Path:   "./data/git",
			Binary: "git",
			Server: "http://localhost:8000",
		},
	}
}

type GitConfig struct {
	Server string
	Code   []byte
	Path   string
	Binary string
}

type GitService struct {
	DB     *bolt.DB
	Config *GitConfig
	Logger *log.Entry
}

func (gs *GitService) Init(app *goapp.App) error {
	os.MkdirAll("."+string(filepath.Separator)+gs.Config.Path, 0755)

	return nil
}

func (gs *GitService) Serve(state *goapp.GoroutineState) error {

	for {
		select {
		case <-state.In:
			return nil
		default:
			gs.SyncRepositories()

			gs.Logger.Info("Wait before starting a new sync...")
			time.Sleep(60 * time.Second)
		}
	}
}

func (gs *GitService) End() error {
	return nil
}

func (gs *GitService) SyncRepositories() {
	// require structure
	// hostname/vendor/project.git
	glob := fmt.Sprintf("%s/*/*/*", gs.Config.Path)
	gs.Logger.WithFields(log.Fields{
		"glob":   glob,
		"action": "sync",
	}).Info("Sync repositories")

	paths, _ := filepath.Glob(glob)

	for _, path := range paths {
		logger := gs.Logger.WithFields(log.Fields{
			"path":   path,
			"action": "fetch",
		})

		path = "." + string(filepath.Separator) + path
		logger.Info("Sync repository")

		cmd := exec.Command(gs.Config.Binary, "fetch")
		cmd.Dir = path

		if err := cmd.Start(); err != nil {
			logger.WithError(err).Error("Error while starting the fetch command")

			continue
		}

		if err := cmd.Wait(); err != nil {
			logger.WithError(err).Error("Error while waiting the fetch command")

			continue
		}

		cmd = exec.Command(gs.Config.Binary, "update-server-info")
		cmd.Dir = path

		if err := cmd.Start(); err != nil {
			logger.WithError(err).Error("Error while starting the update-server-info command")

			continue
		}

		if err := cmd.Wait(); err != nil {
			logger.WithError(err).Error("Error while waiting the update-server-info command")

			continue
		}

		gs.Logger.WithFields(log.Fields{
			"path":   path,
			"action": "sync",
		}).Info("Complete the fetch and update-server-info commands")
	}
}

func (gs *GitService) WriteArchive(w io.Writer, path, ref string) error {
	logger := gs.Logger.WithFields(log.Fields{
		"path":   path,
		"action": "archive",
	})

	cmd := exec.Command(gs.Config.Binary, "archive", "--format=zip", ref)
	cmd.Dir = gs.Config.Path + string(filepath.Separator) + path

	stdout, _ := cmd.StdoutPipe()

	if err := cmd.Start(); err != nil {
		logger.WithError(err).Error("Error while starting the archive command")

		return err
	}

	if _, err := io.Copy(w, stdout); err != nil {
		logger.WithError(err).Error("Error while reading stdout from the archive command")
	}

	if err := cmd.Wait(); err != nil {
		logger.WithError(err).Error("Error while waiting the archive command")

		return err
	}

	logger.Info("Complete the archive command")

	return nil
}

func (gs *GitService) WriteFile(w io.Writer, path string) error {
	logger := gs.Logger.WithFields(log.Fields{
		"path":   path,
		"action": "fetch",
	})

	if f, err := os.Open(gs.Config.Path + string(filepath.Separator) + path); err != nil {
		logger.WithError(err).Error("Error while reading file from the fetch command")

		return err
	} else {
		defer f.Close()

		logger.Debug("Sending data to writer")

		io.Copy(w, f)
	}

	return nil
}

func GitRewriteArchive(config *GitConfig, path string) string {
	if results := GITHUB_ARCHIVE.FindStringSubmatch(path); len(results) == 6 {
		return fmt.Sprintf("%s/git/%s/%s/%s/%s.zip", config.Server, results[2], results[3], results[4], results[5])
	}

	if results := BITBUCKET_ARCHIVE.FindStringSubmatch(path); len(results) == 6 {
		return fmt.Sprintf("%s/git/%s/%s/%s/%s.zip", config.Server, results[2], results[3], results[4], results[5])
	}

	if results := GITLAB_ARCHIVE.FindStringSubmatch(path); len(results) == 6 {
		return fmt.Sprintf("%s/git/%s/%s/%s/%s.zip", config.Server, results[2], results[3], results[4], results[5])
	}

	return config.Server
}

func GitRewriteRepository(config *GitConfig, path string) string {

	if results := SVN_REPOSITORY.FindStringSubmatch(path); len(results) > 1 {
		return path // svn not supported
	}

	if results := GIT_REPOSITORY.FindStringSubmatch(path); len(results) > 1 {
		return fmt.Sprintf("%s/git/%s/%s.git", config.Server, results[6], results[8])
	}

	return config.Server
}
