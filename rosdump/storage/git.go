package storage

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"git.ecadlabs.com/ecad/rostools/rosdump/config"
	"git.ecadlabs.com/ecad/rostools/rosdump/sshutils"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"gopkg.in/src-d/go-billy.v4/memfs"
	"gopkg.in/src-d/go-git.v4"
	gitconfig "gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"gopkg.in/src-d/go-git.v4/plumbing/transport"
	httptransport "gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	sshtransport "gopkg.in/src-d/go-git.v4/plumbing/transport/ssh"
	"gopkg.in/src-d/go-git.v4/storage/memory"
)

type GitStorageConfig struct {
	// Local repository path
	RepositoryPath string
	URL            string
	Pull           bool
	Username       string
	Password       string
	PemBytes       []byte

	// Name of the remote to be pulled. If empty, uses the default.
	RemoteName string
	// Remote branch to clone. If empty, uses HEAD.
	ReferenceName string
	Push          bool
	// RefSpecs specify what destination ref to update with what source
	// object. A refspec with empty src can be used to delete a reference.
	RefSpecs []string

	// Target path template relative to work tree
	DestinationPath string

	// Author name
	Name string
	// Author email
	Email         string
	CommitMessage string

	keyData []byte
}

type GitStorage struct {
	repo    *git.Repository
	conf    *GitStorageConfig
	destTpl *template.Template
	msgTpl  *template.Template
	mtx     sync.Mutex
	logger  *logrus.Logger
}

var errCloneURL = errors.New("git: clone URL must be specified")

func (g *GitStorageConfig) authMethod() (transport.AuthMethod, error) {
	u, err := url.Parse(g.URL)
	if err != nil {
		u, err = url.Parse("ssh://" + g.URL)
		if err != nil {
			return nil, err
		}
	}

	username := g.Username
	password := g.Password

	if n := u.User.Username(); n != "" {
		username = n
		if p, ok := u.User.Password(); ok {
			password = p
		}
	}

	if strings.HasPrefix(u.Scheme, "http") {
		return &httptransport.BasicAuth{
			Username: username,
			Password: password,
		}, nil
	} else {
		if g.PemBytes != nil {
			res, err := sshtransport.NewPublicKeys(username, g.PemBytes, g.Password)
			if err != nil {
				return nil, err
			}

			res.HostKeyCallback = ssh.InsecureIgnoreHostKey()
			return res, nil
		}

		return &sshtransport.Password{
			User:     username,
			Password: password,
			HostKeyCallbackHelper: sshtransport.HostKeyCallbackHelper{
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			},
		}, nil
	}
}

func (g *GitStorageConfig) cloneOptions() (*git.CloneOptions, error) {
	auth, err := g.authMethod()
	if err != nil {
		return nil, err
	}

	return &git.CloneOptions{
		RemoteName:    g.RemoteName,
		ReferenceName: plumbing.ReferenceName(g.ReferenceName),
		URL:           g.URL,
		Auth:          auth,
	}, nil
}

func initFS(ctx context.Context, conf *GitStorageConfig, logger *logrus.Logger) (*git.Repository, error) {
	logger.Infoln("using existing local Git repository")

	auth, err := conf.authMethod()
	if err != nil {
		return nil, err
	}

	repo, err := git.PlainOpen(conf.RepositoryPath)
	if err == nil {
		if conf.Pull {
			progress := logger.Writer()
			defer progress.Close()

			opts := git.PullOptions{
				RemoteName:    conf.RemoteName,
				ReferenceName: plumbing.ReferenceName(conf.ReferenceName),
				Auth:          auth,
				Progress:      progress,
			}

			wt, err := repo.Worktree()
			if err != nil {
				return nil, err
			}

			logger.Infoln("pulling...")

			if err := wt.PullContext(ctx, &opts); err != nil {
				if err == git.NoErrAlreadyUpToDate {
					logger.Infoln(err)
				} else {
					return nil, err
				}
			}
		}

		return repo, nil
	}

	if err == git.ErrRepositoryNotExists {
		if conf.URL == "" {
			return nil, errCloneURL
		}

		logger.WithFields(logrus.Fields{
			"repository": conf.RepositoryPath,
			"url":        conf.URL,
		}).Infoln("cloning...")

		opt, err := conf.cloneOptions()
		if err != nil {
			return nil, err
		}

		progress := logger.Writer()
		defer progress.Close()

		opt.Progress = progress

		return git.PlainCloneContext(ctx, conf.RepositoryPath, false, opt)
	}

	return nil, err
}

func initMem(ctx context.Context, conf *GitStorageConfig, logger *logrus.Logger) (*git.Repository, error) {
	if conf.URL == "" {
		return nil, errCloneURL
	}

	logger.WithField("url", conf.URL).Infoln("cloning into memory storage...")

	wt := memfs.New()
	dot := memory.NewStorage()

	opt, err := conf.cloneOptions()
	if err != nil {
		return nil, err
	}

	progress := logger.Writer()
	defer progress.Close()

	opt.Progress = progress

	return git.CloneContext(ctx, dot, wt, opt)
}

func NewGitStorage(ctx context.Context, conf *GitStorageConfig, logger *logrus.Logger) (*GitStorage, error) {
	var (
		repo *git.Repository
		err  error
	)

	if conf.RepositoryPath != "" {
		repo, err = initFS(ctx, conf, logger)
	} else {
		repo, err = initMem(ctx, conf, logger)
	}

	if err != nil {
		return nil, fmt.Errorf("git: %v", err)
	}

	destTpl, err := template.New("destination").Parse(conf.DestinationPath)
	if err != nil {
		return nil, fmt.Errorf("git: %v", err)
	}

	msgTpl, err := template.New("message").Parse(conf.CommitMessage)
	if err != nil {
		return nil, fmt.Errorf("git: %v", err)
	}

	return &GitStorage{
		repo:    repo,
		conf:    conf,
		destTpl: destTpl,
		msgTpl:  msgTpl,
		logger:  logger,
	}, nil
}

type gitStorageTx struct {
	wt        *git.Worktree
	g         *GitStorage
	timestamp time.Time
}

func (g *GitStorage) Begin(ctx context.Context) (Tx, error) {
	wt, err := g.repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("git: %v", err)
	}

	return &gitStorageTx{
		g:         g,
		wt:        wt,
		timestamp: time.Now(),
	}, nil
}

func (g *gitStorageTx) Add(ctx context.Context, metadata map[string]interface{}, stream io.Reader) error {
	var dest strings.Builder
	if err := g.g.destTpl.Execute(&dest, metadata); err != nil {
		return fmt.Errorf("git: %v", err)
	}

	out := dest.String()

	g.g.mtx.Lock()
	defer g.g.mtx.Unlock()

	// Use underlying FS abstraction
	fs := g.wt.Filesystem

	dir := path.Dir(out)
	if err := fs.MkdirAll(dir, 0777); err != nil {
		return fmt.Errorf("git: %v", err)
	}

	g.g.logger.WithField("file", out).Infoln("writing...")

	fd, err := fs.Create(out)
	if err != nil {
		return fmt.Errorf("git: %v", err)
	}

	if _, err := io.Copy(fd, stream); err != nil {
		return fmt.Errorf("git: %v", err)
	}

	if err := fd.Close(); err != nil {
		return fmt.Errorf("git: %v", err)
	}

	if _, err := g.wt.Add(out); err != nil {
		return fmt.Errorf("git: %v", err)
	}

	return nil
}

func (g *gitStorageTx) Timestamp() time.Time { return g.timestamp }

func (g *gitStorageTx) Commit(ctx context.Context) error {
	tdata := map[string]interface{}{
		"time": g.timestamp,
	}

	var msg strings.Builder
	if err := g.g.msgTpl.Execute(&msg, tdata); err != nil {
		return fmt.Errorf("git: %v", err)
	}

	g.g.mtx.Lock()
	defer g.g.mtx.Unlock()

	commit, err := g.wt.Commit(msg.String(), &git.CommitOptions{
		Author: &object.Signature{
			Name:  g.g.conf.Name,
			Email: g.g.conf.Email,
			When:  time.Now(),
		},
	})
	if err != nil {
		return fmt.Errorf("git: %v", err)
	}

	g.g.logger.WithFields(logrus.Fields{
		"hash": commit.String(),
		"msg":  msg.String(),
	}).Infoln("committing...")

	if _, err := g.g.repo.CommitObject(commit); err != nil {
		return fmt.Errorf("git: %v", err)
	}

	//g.g.logger.Infoln(obj)

	if g.g.conf.Push {
		g.g.logger.Infoln("pushing...")

		auth, err := g.g.conf.authMethod()
		if err != nil {
			return fmt.Errorf("git: %v", err)
		}

		progress := g.g.logger.Writer()
		defer progress.Close()

		opts := git.PushOptions{
			RemoteName: g.g.conf.RemoteName,
			Auth:       auth,
			Progress:   progress,
		}

		if len(g.g.conf.RefSpecs) != 0 {
			opts.RefSpecs = make([]gitconfig.RefSpec, len(g.g.conf.RefSpecs))
			for i, v := range g.g.conf.RefSpecs {
				opts.RefSpecs[i] = gitconfig.RefSpec(v)
			}
		}

		if err := g.g.repo.PushContext(ctx, &opts); err != nil {
			return fmt.Errorf("git: %v", err)
		}
	}

	return nil
}

func newGitStorage(ctx context.Context, options config.Options, logger *logrus.Logger) (Storage, error) {
	var conf GitStorageConfig
	conf.RepositoryPath, _ = options.GetString("repository_path")
	conf.URL, _ = options.GetString("url")
	conf.Pull, _ = options.GetBool("pull")
	conf.Username, _ = options.GetString("username")
	conf.Password, _ = options.GetString("password")

	if name, err := options.GetString("identity_file"); err == nil && name != "" {
		pem, err := sshutils.ReadIdentityFile(name)
		if err != nil {
			return nil, fmt.Errorf("git: %v", err)
		}
		conf.PemBytes = pem
	}

	conf.RemoteName, _ = options.GetString("remote_name")
	conf.ReferenceName, _ = options.GetString("reference_name")
	conf.Push, _ = options.GetBool("push")

	if v, ok := options["ref_specs"]; ok {
		switch vv := v.(type) {
		case []interface{}:
			for _, iv := range vv {
				if s, ok := iv.(string); ok {
					conf.RefSpecs = append(conf.RefSpecs, s)
				}
			}

		case string:
			conf.RefSpecs = []string{vv}
		}

		fmt.Println(conf.RefSpecs)
	}

	conf.DestinationPath, _ = options.GetString("destination_path")
	conf.Name, _ = options.GetString("name")
	conf.Email, _ = options.GetString("email")
	conf.CommitMessage, _ = options.GetString("commit_message")

	return NewGitStorage(ctx, &conf, logger)
}

func init() {
	registerStorage("git", newGitStorage)
}
