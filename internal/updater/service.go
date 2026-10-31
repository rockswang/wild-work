package updater

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Config 自动更新器配置。
type Config struct {
	CurrentVersion string
	Repo           string
	Proxies        []string
	AutoCheck      bool
	CheckInterval  time.Duration
}

// Service 自动更新管理服务。
type Service struct {
	cfg          Config
	metaClient   *http.Client
	streamClient *http.Client
	mu           sync.Mutex
	state        State
	cancelDl     context.CancelFunc
	dismissed    string
	downloaded   string
}

// NewService 创建自动更新管理器。
// 遵循 R35 纪律：为元数据探测与大文件流式下载分别提供 Client。
func NewService(cfg Config, metaClient, streamClient *http.Client) *Service {
	if metaClient == nil {
		metaClient = &http.Client{Timeout: 10 * time.Second}
	}
	if streamClient == nil {
		streamClient = &http.Client{} // R35: 无全局 Timeout
	} else if streamClient.Timeout != 0 {
		c := *streamClient
		c.Timeout = 0
		streamClient = &c
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 6 * time.Hour
	}
	if cfg.Repo == "" {
		cfg.Repo = "rockswang/wild-work"
	}

	return &Service{
		cfg:          cfg,
		metaClient:   metaClient,
		streamClient: streamClient,
		state: State{
			Status:         StatusIdle,
			CurrentVersion: CleanVersion(cfg.CurrentVersion),
		},
	}
}

// UpdateConfig 动态更新配置（如代理列表或仓库名变更）。
func (s *Service) UpdateConfig(proxies []string, repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(proxies) > 0 {
		s.cfg.Proxies = proxies
	}
	if repo != "" {
		s.cfg.Repo = repo
	}
}

// GetState 获取当前自动更新状态快照。
func (s *Service) GetState() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	if s.dismissed != "" && st.LatestVersion == s.dismissed && st.Status == StatusAvailable {
		// 用户主动忽略了该版本的提示
		st.HasUpdate = false
	}
	return st
}

// Check 执行版本检查。manual 表示是否为用户手动触发。
func (s *Service) Check(ctx context.Context, manual bool) (*State, error) {
	s.mu.Lock()
	if s.state.Status == StatusDownloading {
		st := s.state
		s.mu.Unlock()
		return &st, nil
	}
	if s.state.Checking {
		st := s.state
		s.mu.Unlock()
		return &st, nil
	}

	s.state.Checking = true
	s.state.Status = StatusChecking
	s.state.Error = ""
	proxies := make([]string, len(s.cfg.Proxies))
	copy(proxies, s.cfg.Proxies)
	repo := s.cfg.Repo
	currVer := s.cfg.CurrentVersion
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.state.Checking = false
		s.mu.Unlock()
	}()

	rel, hasUpdate, err := CheckSources(ctx, s.metaClient, repo, currVer, proxies)
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state.LastCheckedAt = time.Now().Format("2006-01-02 15:04:05")

	if err != nil {
		if manual {
			s.state.Status = StatusFailed
			s.state.Error = err.Error()
			st := s.state
			return &st, err
		}
		// 后台静默失败
		log.Printf("[updater] background version check failed: %v", err)
		if s.state.Status == StatusChecking {
			s.state.Status = StatusIdle
		}
		st := s.state
		return &st, nil
	}

	s.state.Release = rel
	s.state.LatestVersion = rel.Version
	s.state.HasUpdate = hasUpdate

	if hasUpdate {
		s.state.Status = StatusAvailable
	} else {
		s.state.Status = StatusIdle
	}

	st := s.state
	return &st, nil
}

// StartDownload 开始下载新版本并校验摘要。
// 下载完成并不立即替换正在运行的程序，而是置为就绪状态，由用户决定重启时机。
func (s *Service) StartDownload() error {
	s.mu.Lock()
	if s.state.Status == StatusDownloading {
		s.mu.Unlock()
		return fmt.Errorf("already downloading")
	}
	if s.state.Release == nil || s.state.Release.Asset == nil {
		s.mu.Unlock()
		return fmt.Errorf("no downloadable release asset available")
	}

	dlCtx, cancel := context.WithCancel(context.Background())
	s.cancelDl = cancel

	asset := *s.state.Release.Asset
	proxies := make([]string, len(s.cfg.Proxies))
	copy(proxies, s.cfg.Proxies)

	s.state.Status = StatusDownloading
	s.state.Downloading = true
	s.state.Progress = 0
	s.state.DownloadedBytes = 0
	s.state.TotalBytes = asset.Size
	s.state.Error = ""
	s.mu.Unlock()

	go func() {
		defer cancel()

		onProgress := func(downloaded, total int64, speed int64, source string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.state.DownloadedBytes = downloaded
			if total > 0 {
				s.state.TotalBytes = total
				s.state.Progress = float64(downloaded) / float64(total) * 100
				if s.state.Progress > 100 {
					s.state.Progress = 100
				}
			}
			s.state.SpeedBytesPerSec = speed
			s.state.Source = source
		}

		res, err := DownloadAsset(dlCtx, s.streamClient, &asset, proxies, onProgress)
		if err != nil {
			s.mu.Lock()
			s.state.Status = StatusFailed
			s.state.Downloading = false
			s.state.Error = fmt.Sprintf("download failed: %v", err)
			s.mu.Unlock()
			return
		}

		s.mu.Lock()
		s.downloaded = res.FilePath
		s.state.Status = StatusReady
		s.state.Downloading = false
		s.state.ReadyToRestart = true
		s.state.Progress = 100
		s.state.Source = res.Source
		s.mu.Unlock()
	}()

	return nil
}

// CancelDownload 取消下载。
func (s *Service) CancelDownload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelDl != nil {
		s.cancelDl()
		s.cancelDl = nil
	}
	s.state.Downloading = false
	s.state.Status = StatusIdle
	s.state.Error = "download cancelled"
}

// Dismiss 忽略当前版本的提示。
func (s *Service) Dismiss(version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dismissed = CleanVersion(version)
}

// GetDownloadedPath 获取已下载校验就绪的更新文件路径。
func (s *Service) GetDownloadedPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.downloaded
}

// StartBackground 启动后台定期检测 goroutine。
func (s *Service) StartBackground(ctx context.Context) {
	if !s.cfg.AutoCheck {
		return
	}

	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}

		_, _ = s.Check(ctx, false)

		interval := s.cfg.CheckInterval
		if interval < 10*time.Minute {
			interval = 6 * time.Hour
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = s.Check(ctx, false)
			}
		}
	}()
}
