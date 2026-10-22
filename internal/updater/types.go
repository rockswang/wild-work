package updater

// Status 自动更新状态枚举。
type Status string

const (
	StatusIdle        Status = "idle"
	StatusChecking    Status = "checking"
	StatusAvailable   Status = "available"
	StatusDownloading Status = "downloading"
	StatusReady       Status = "ready"
	StatusFailed      Status = "failed"
)

// AssetInfo 单个 Release 文件资产信息。
type AssetInfo struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Digest      string `json:"digest"`
	DownloadURL string `json:"download_url"`
}

// ReleaseInfo GitHub Release 元数据。
type ReleaseInfo struct {
	TagName     string      `json:"tag_name"`
	Version     string      `json:"version"`
	Title       string      `json:"title"`
	Changelog   string      `json:"changelog"`
	PublishedAt string      `json:"published_at"`
	HTMLURL     string      `json:"html_url,omitempty"`
	Asset       *AssetInfo  `json:"asset,omitempty"`
	AllAssets   []AssetInfo `json:"assets,omitempty"`
}

// State 暴露给前端面板的当前更新状态。
type State struct {
	Status           Status       `json:"status"`
	CurrentVersion   string       `json:"current_version"`
	LatestVersion    string       `json:"latest_version,omitempty"`
	Release          *ReleaseInfo `json:"release,omitempty"`
	HasUpdate        bool         `json:"has_update"`
	Checking         bool         `json:"checking"`
	Downloading      bool         `json:"downloading"`
	ReadyToRestart   bool         `json:"ready_to_restart"`
	Progress         float64      `json:"progress"`
	SpeedBytesPerSec int64        `json:"speed_bytes_per_sec"`
	DownloadedBytes  int64        `json:"downloaded_bytes"`
	TotalBytes       int64        `json:"total_bytes"`
	Source           string       `json:"source,omitempty"`
	Error            string       `json:"error,omitempty"`
	LastCheckedAt    string       `json:"last_checked_at,omitempty"`
}