// 移植自 C（MEIZU16）的 utils/pow.py：网页版 sentinel 的 requirements token 与 PoW。
//
// 与原实现的差异只在性能：把 25 字段 config 的 JSON 拆成「静态前缀 + 计数 + 静态中段 +
// 耗时 + 静态后缀」，每轮只拼两个整数，避免整份重新序列化（实测快 20–25 倍）。
package pow

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultPowScript   = "https://chatgpt.com/backend-api/sentinel/sdk.js"
	DefaultHeapLimit   = 4395630592
	DefaultLanguage    = "zh-CN"
	DefaultLanguages   = "en-US,en"
	AnswerSuffix       = "~S"
	legacyRequirements = "gAAAAAC"
	proofPrefix        = "gAAAAAB"
)

var (
	Cores             = []int{8, 16, 24, 32}
	DocumentKeys      = []string{"__reactContainer$fzelfjyxej8", "_reactListening5dehydibo78", "location"}
	ScreenResolutions = [][2]int{{1920, 1080}, {1440, 900}, {2560, 1440}, {3840, 2160}}
)

var navigatorKeys = []string{
	"registerProtocolHandler−function registerProtocolHandler() { [native code] }",
	"storage−[object StorageManager]",
	"locks−[object LockManager]",
	"appCodeName−Mozilla",
	"permissions−[object Permissions]",
	"share−function share() { [native code] }",
	"webdriver−false",
	"managed−[object NavigatorManagedData]",
	"canShare−function canShare() { [native code] }",
	"vendor−Google Inc.",
	"mediaDevices−[object MediaDevices]",
	"vibrate−function vibrate() { [native code] }",
	"storageBuckets−[object StorageBucketManager]",
	"mediaCapabilities−[object MediaCapabilities]",
	"cookieEnabled−true",
	"virtualKeyboard−[object VirtualKeyboard]",
	"product−Gecko",
	"presentation−[object Presentation]",
	"onLine−true",
	"mimeTypes−[object MimeTypeArray]",
	"credentials−[object CredentialsContainer]",
	"serviceWorker−[object ServiceWorkerContainer]",
	"keyboard−[object Keyboard]",
	"gpu−[object GPU]",
	"doNotTrack",
	"serial−[object Serial]",
	"pdfViewerEnabled−true",
	"language−zh-CN",
	"geolocation−[object Geolocation]",
	"userAgentData−[object NavigatorUAData]",
	"getUserMedia−function getUserMedia() { [native code] }",
	"sendBeacon−function sendBeacon() { [native code] }",
	"hardwareConcurrency−32",
	"windowControlsOverlay−[object WindowControlsOverlay]",
}

var windowKeys = []string{
	"0", "window", "self", "document", "name", "location", "customElements", "history",
	"navigation", "innerWidth", "innerHeight", "scrollX", "scrollY", "visualViewport",
	"screenX", "screenY", "outerWidth", "outerHeight", "devicePixelRatio", "screen",
	"chrome", "navigator", "onresize", "performance", "crypto", "indexedDB",
	"sessionStorage", "localStorage", "scheduler", "alert", "atob", "btoa", "fetch",
	"matchMedia", "postMessage", "queueMicrotask", "requestAnimationFrame", "setInterval",
	"setTimeout", "caches", "__NEXT_DATA__", "__BUILD_MANIFEST", "__NEXT_PRELOADREADY",
}

func pick(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// newUUID 对应 utils.helper.new_uuid()（uuid4）
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// browserDate 复刻浏览器 new Date().toString()；有 environment 时用它的时区，否则用本机时区。
func browserDate(env map[string]any) string {
	suffix, _ := env["date_suffix"].(string)
	off, hasOff := env["utc_offset_minutes"].(float64)
	if oi, ok := env["utc_offset_minutes"].(int); ok {
		off, hasOff = float64(oi), true
	}
	var now time.Time
	if suffix != "" && hasOff {
		now = time.Now().UTC().Add(time.Duration(int(off)) * time.Minute)
	} else {
		now = time.Now()
		_, secs := now.Zone()
		// 复刻 Python 的 %z 与 tzname()：+0800 与 China Standard Time
		sign := "+"
		if secs < 0 {
			sign, secs = "-", -secs
		}
		suffix = fmt.Sprintf("GMT%s%02d%02d (%s)", sign, secs/3600, (secs%3600)/60, zoneName(now))
	}
	return now.Format("Mon Jan 02 15:04:05 2006") + " " + suffix
}

func zoneName(t time.Time) string {
	name, _ := t.Zone()
	return name
}

// config 是一份 25 字段的浏览器指纹（index 见 C 的 utils/pow.py:70-188）
func config(ua string, scriptSources []string, dataBuild string, env map[string]any) []any {
	screenSum := 0
	if v, ok := numOf(env["screen_sum"]); ok {
		screenSum = v
	} else {
		r := ScreenResolutions[pick(len(ScreenResolutions))]
		screenSum = r[0] + r[1]
	}
	heap := DefaultHeapLimit
	if v, ok := numOf(env["heap_limit"]); ok {
		heap = v
	}
	lang := DefaultLanguage
	if v, ok := env["language"].(string); v != "" && ok {
		lang = v
	}
	langs := DefaultLanguages
	if v, ok := env["languages"].(string); v != "" && ok {
		langs = v
	}
	cores := Cores[pick(len(Cores))]
	if v, ok := numOf(env["hardware_concurrency"]); ok {
		cores = v
	}
	script := DefaultPowScript
	if len(scriptSources) > 0 {
		script = scriptSources[pick(len(scriptSources))]
	}
	pageAge := 2000 + float64(pick(28000)) + float64(pick(1000))/1000 // uniform(2000,30000) 近似
	return []any{
		screenSum,                               // 0  screen.width + screen.height
		browserDate(env),                        // 1  new Date().toString()
		heap,                                    // 2  performance.memory.jsHeapSizeLimit
		1,                                       // 3  计数器（求解时被覆盖）
		ua,                                      // 4  navigator.userAgent
		script,                                  // 5  sdk script src
		dataBuild,                               // 6  html data-build
		lang,                                    // 7  navigator.language
		langs,                                   // 8  navigator.languages
		0.5,                                     // 9  耗时 ms（求解时被覆盖）
		navigatorKeys[pick(len(navigatorKeys))], // 10 navigator 探测键
		DocumentKeys[pick(len(DocumentKeys))],   // 11 document 探测键
		windowKeys[pick(len(windowKeys))],       // 12 window 探测键
		pageAge,                                 // 13 页面打开以来毫秒
		newUUID(),                               // 14 页面 uuid
		"",                                      // 15 保留
		cores,                                   // 16 hardwareConcurrency
		float64(time.Now().UnixMilli()) - pageAge, // 17 打开页面的墙钟
		0, 0, 0, 0, 0, 0, // 18-23
		0, // 24 0=edge/chrome, 1=firefox
	}
}

func numOf(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case float64:
		return int(x), true
	case int64:
		return int(x), true
	}
	return 0, false
}

func marshal(v any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false) // Python 的 json.dumps 不转义 <>&
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func encodeConfig(c []any) string {
	return base64.StdEncoding.EncodeToString([]byte(marshal(c)))
}

// BuildLegacyRequirementsToken 对应 build_legacy_requirements_token：返回 "gAAAAAC"+base64(config)
func BuildLegacyRequirementsToken(ua string, scriptSources []string, dataBuild string, env map[string]any) string {
	return legacyRequirements + encodeConfig(config(ua, scriptSources, dataBuild, env))
}

// fnv1a32 是网页 sentinel SDK 的 PoW 哈希：FNV-1a 32 位 + 一轮 avalanche。
// 注意必须按 Unicode 码点迭代（Python 的 ord(char)），用 []byte 会让中文静默算错。
func fnv1a32(text string) string {
	value := uint32(2166136261)
	for _, char := range text {
		value ^= uint32(char)
		value *= 16777619
	}
	value ^= value >> 16
	value *= 2246822507
	value ^= value >> 13
	value *= 3266489909
	value ^= value >> 16
	return fmt.Sprintf("%08x", value)
}

// skeleton 把 config 的 JSON 拆成「前缀 + 计数 + 中段 + 耗时 + 后缀」，
// 每轮只替换两个整数，避免整份重新序列化。
type skeleton struct {
	head, mid, tail string
}

func newSkeleton(c []any) skeleton {
	parts := make([]string, len(c))
	for i, v := range c {
		parts[i] = marshal(v)
	}
	return skeleton{
		head: "[" + strings.Join(parts[0:3], ",") + ",",
		mid:  "," + strings.Join(parts[4:9], ",") + ",",
		tail: "," + strings.Join(parts[10:], ",") + "]",
	}
}

func (s skeleton) render(count int, ms int) string {
	return s.head + strconv.Itoa(count) + s.mid + strconv.Itoa(ms) + s.tail
}

// Solve 对应 build_proof_token 的内层 _pow_generate：返回 "gAAAAAB"+answer。
// limit 与 C 一致为 500000。
func Solve(seed, difficulty, ua string, env map[string]any) (string, bool) {
	return SolveWith(seed, difficulty, ua, nil, "", env, 500000)
}

func SolveWith(seed, difficulty, ua string, scriptSources []string, dataBuild string, env map[string]any, limit int) (string, bool) {
	sk := newSkeleton(config(ua, scriptSources, dataBuild, env))
	started := time.Now()
	dl := len(difficulty)
	for i := 0; i < limit; i++ {
		ms := int(time.Since(started).Milliseconds())
		encoded := base64.StdEncoding.EncodeToString([]byte(sk.render(i, ms)))
		if fnv1a32(seed + encoded)[:dl] <= difficulty {
			return proofPrefix + encoded + AnswerSuffix, true
		}
	}
	return "", false
}
