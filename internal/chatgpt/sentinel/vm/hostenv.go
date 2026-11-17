package vm

// 宿主环境模拟：挑战程序（dx 解出的那 86~90 条指令）会读写一批真实页面对象，
// 它们是「浏览器 API」而不是「VM 算法」。要让纯 Go VM 算出上游接受的 token，
// 就必须把这层页面对象补出来——C（MEIZU16）用 Playwright 起真页面解决同一个问题。
//
// 本文件按真实 Chrome 页面的形状构造这些对象。所有函数都写成 func(...any) any，
// 由 op7/op17/op24 通过 map 查表调用（与 JS 里 `obj.method(...)` 同构）。

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// Env 描述模拟页面所处的环境（UA、屏幕、时区等），决定指纹类返回值
type Env struct {
	UserAgent      string
	Language       string
	Languages      string
	HardwareConcur int
	ScreenW        int
	ScreenH        int
	HeapLimit      float64
	TimezoneOffset int // 分钟，东八区为 -480（web client 约定）
}

// DefaultEnv 返回一份常见的 Windows/Chrome 环境
func DefaultEnv() Env {
	return Env{
		UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
		Language:       "zh-CN",
		Languages:      "zh-CN,zh,en",
		HardwareConcur: 8,
		ScreenW:        1920,
		ScreenH:        1080,
		HeapLimit:      4395630592,
		TimezoneOffset: -480,
	}
}

// NewWindow 构造寄存器 10（window）的值
func NewWindow(e Env) any {
	store := map[string]any{} // localStorage 的底层存储
	w := map[string]any{}

	w["navigator"] = map[string]any{
		"userAgent":           e.UserAgent,
		"appVersion":          strings.TrimPrefix(e.UserAgent, "Mozilla/"),
		"appName":             "Netscape",
		"appCodeName":         "Mozilla",
		"product":             "Gecko",
		"productSub":          "20030107",
		"vendor":              "Google Inc.",
		"vendorSub":           "",
		"platform":            "Win32",
		"language":            e.Language,
		"languages":           strings.Split(e.Languages, ","),
		"hardwareConcurrency": float64(e.HardwareConcur),
		"deviceMemory":        float64(8),
		"maxTouchPoints":      float64(0),
		"cookieEnabled":       true,
		"onLine":              true,
		"doNotTrack":          undef,
		"webdriver":           undef, // 关键：真页面上是 undefined
		"pdfViewerEnabled":    true,
		"storage": map[string]any{
			"estimate": func(a ...any) any {
				return map[string]any{"quota": float64(3e11), "usage": float64(1.2e6)}
			},
			"persisted": func(a ...any) any { return false },
		},
		"connection": map[string]any{"effectiveType": "4g", "rtt": float64(50), "downlink": float64(10)},
	}
	w["screen"] = map[string]any{
		"width":       float64(e.ScreenW),
		"height":      float64(e.ScreenH),
		"availWidth":  float64(e.ScreenW),
		"availHeight": float64(e.ScreenH - 40), // 任务栏
		"colorDepth":  float64(24),
		"pixelDepth":  float64(24),
		"availLeft":   float64(0),
		"availTop":    float64(0),
		"orientation": map[string]any{"type": "landscape-primary", "angle": float64(0)},
	}
	w["document"] = map[string]any{
		"body":            map[string]any{"clientWidth": float64(e.ScreenW), "clientHeight": float64(e.ScreenH - 40), "tagName": "BODY"},
		"documentElement": map[string]any{"clientWidth": float64(e.ScreenW), "clientHeight": float64(e.ScreenH - 40), "tagName": "HTML"},
		"title":           "ChatGPT",
		"URL":             "https://chatgpt.com/",
		"referrer":        "",
		"cookie":          "",
		"readyState":      "complete",
		"hidden":          false,
		"visibilityState": "visible",
		"characterSet":    "UTF-8",
		"compatMode":      "CSS1Compat",
		"scripts": []any{ // 首页 bundle 动态插入的 sentinel sdk（挑战程序按正则找版本号）
			map[string]any{"src": "https://chatgpt.com/sentinel/20260219f9f6/sdk.js"},
		},
		"createElement":        func(a ...any) any { return newElement(jsStr(arg(a, 0)), e) },
		"getElementById":       func(a ...any) any { return newElement("div", e) },
		"querySelector":        func(a ...any) any { return newElement("div", e) },
		"querySelectorAll":     func(a ...any) any { return []any{} },
		"getElementsByTagName": func(a ...any) any { return []any{} },
		"createTextNode":       func(a ...any) any { return map[string]any{"textContent": jsStr(arg(a, 0))} },
		"hasFocus":             func(a ...any) any { return true },
	}
	w["history"] = map[string]any{"length": float64(2), "state": nil, "scrollRestoration": "auto"}
	w["location"] = map[string]any{
		"href": "https://chatgpt.com/", "origin": "https://chatgpt.com", "protocol": "https:",
		"host": "chatgpt.com", "hostname": "chatgpt.com", "port": "", "pathname": "/",
		"search": "", "hash": "",
	}
	w["localStorage"] = map[string]any{
		"getItem": func(a ...any) any {
			if v, ok := store[jsStr(arg(a, 0))]; ok {
				return v
			}
			return nil
		},
		"setItem":    func(a ...any) any { store[jsStr(arg(a, 0))] = jsStr(arg(a, 1)); return undef },
		"removeItem": func(a ...any) any { delete(store, jsStr(arg(a, 0))); return undef },
		"clear":      func(a ...any) any { store = map[string]any{}; return undef },
		"key":        func(a ...any) any { return nil },
		"length":     float64(0),
	}
	w["sessionStorage"] = w["localStorage"]
	w["performance"] = map[string]any{
		"now":              func(a ...any) any { return float64(time.Now().UnixNano()) / 1e6 },
		"timeOrigin":       float64(time.Now().UnixMilli()),
		"memory":           map[string]any{"jsHeapSizeLimit": e.HeapLimit, "usedJSHeapSize": float64(3.2e7), "totalJSHeapSize": float64(5.6e7)},
		"getEntriesByType": func(a ...any) any { return []any{} },
	}
	w["Math"] = map[string]any{
		"abs":    func(a ...any) any { return math.Abs(jsNum(arg(a, 0))) },
		"floor":  func(a ...any) any { return math.Floor(jsNum(arg(a, 0))) },
		"ceil":   func(a ...any) any { return math.Ceil(jsNum(arg(a, 0))) },
		"round":  func(a ...any) any { return math.Round(jsNum(arg(a, 0))) },
		"max":    func(a ...any) any { return math.Max(jsNum(arg(a, 0)), jsNum(arg(a, 1))) },
		"min":    func(a ...any) any { return math.Min(jsNum(arg(a, 0)), jsNum(arg(a, 1))) },
		"pow":    func(a ...any) any { return math.Pow(jsNum(arg(a, 0)), jsNum(arg(a, 1))) },
		"sqrt":   func(a ...any) any { return math.Sqrt(jsNum(arg(a, 0))) },
		"random": func(a ...any) any { return rand.Float64() },
		"PI":     math.Pi,
		"E":      math.E,
	}
	// Reflect.set(target, key, value)：挑战程序用它往对象上写属性
	w["Reflect"] = map[string]any{
		"set": func(a ...any) any {
			obj, key, val := arg(a, 0), arg(a, 1), arg(a, 2)
			if propSet(obj, key, val) {
				return true
			}
			return false
		},
		"get": func(a ...any) any { return propGet(arg(a, 0), arg(a, 1)) },
		"has": func(a ...any) any {
			if m, ok := arg(a, 0).(map[string]any); ok {
				_, has := m[jsStr(arg(a, 1))]
				return has
			}
			return false
		},
		"deleteProperty": func(a ...any) any {
			if m, ok := arg(a, 0).(map[string]any); ok {
				delete(m, jsStr(arg(a, 1)))
			}
			return true
		},
		"ownKeys": func(a ...any) any {
			out := []any{}
			if m, ok := arg(a, 0).(map[string]any); ok {
				for k := range m {
					out = append(out, k)
				}
			}
			return out
		},
	}
	// Object.create(proto)：返回一个新的空对象
	w["Object"] = map[string]any{
		"create": func(a ...any) any { return map[string]any{} },
		"keys": func(a ...any) any {
			out := []any{}
			if m, ok := arg(a, 0).(map[string]any); ok {
				for k := range m {
					out = append(out, k)
				}
			}
			return out
		},
		"assign": func(a ...any) any {
			dst, _ := arg(a, 0).(map[string]any)
			for i := 1; i < len(a); i++ {
				if src, ok := a[i].(map[string]any); ok {
					for k, v := range src {
						dst[k] = v
					}
				}
			}
			return dst
		},
		"entries": func(a ...any) any { return []any{} },
		"freeze":  func(a ...any) any { return arg(a, 0) },
		"defineProperty": func(a ...any) any {
			propSet(arg(a, 0), arg(a, 1), propGet(arg(a, 2), "value"))
			return arg(a, 0)
		},
		"getOwnPropertyNames": w["Reflect"].(map[string]any)["ownKeys"],
	}
	w["Date"] = map[string]any{
		"now": func(a ...any) any { return float64(time.Now().UnixMilli()) },
	}
	w["Array"] = map[string]any{
		"isArray": func(a ...any) any { _, ok := arg(a, 0).([]any); return ok },
		"from": func(a ...any) any {
			if arr, ok := arg(a, 0).([]any); ok {
				return append([]any{}, arr...)
			}
			return []any{}
		},
	}
	w["JSON"] = map[string]any{
		"parse":     func(a ...any) any { var out any; _ = json.Unmarshal([]byte(jsStr(arg(a, 0))), &out); return out },
		"stringify": func(a ...any) any { b, _ := json.Marshal(jsonable(arg(a, 0))); return string(b) },
	}
	w["Number"] = map[string]any{
		"parseInt": func(a ...any) any {
			f, _ := strconv.ParseFloat(strings.TrimSpace(jsStr(arg(a, 0))), 64)
			return math.Trunc(f)
		},
		"parseFloat": func(a ...any) any { f, _ := strconv.ParseFloat(strings.TrimSpace(jsStr(arg(a, 0))), 64); return f },
		"isNaN":      func(a ...any) any { return math.IsNaN(jsNum(arg(a, 0))) },
		"isFinite":   func(a ...any) any { f := jsNum(arg(a, 0)); return !math.IsInf(f, 0) && !math.IsNaN(f) },
	}
	w["String"] = map[string]any{
		"fromCharCode": func(a ...any) any {
			var sb strings.Builder
			for i := range a {
				sb.WriteRune(rune(int(jsNum(a[i]))))
			}
			return sb.String()
		},
	}
	w["chrome"] = map[string]any{"runtime": map[string]any{}, "app": map[string]any{}}
	w["innerWidth"] = float64(e.ScreenW)
	w["innerHeight"] = float64(e.ScreenH - 120)
	w["outerWidth"] = float64(e.ScreenW)
	w["outerHeight"] = float64(e.ScreenH)
	w["devicePixelRatio"] = float64(1)
	w["isSecureContext"] = true
	w["origin"] = "https://chatgpt.com"
	w["name"] = ""
	w["closed"] = false
	w["atob"] = func(a ...any) any { return atob(jsStr(arg(a, 0))) }
	w["btoa"] = func(a ...any) any { return b64(jsStr(arg(a, 0))) }
	w["setTimeout"] = func(a ...any) any { return float64(0) }
	w["clearTimeout"] = func(a ...any) any { return undef }
	w["requestAnimationFrame"] = func(a ...any) any { return float64(0) }
	w["cancelAnimationFrame"] = func(a ...any) any { return undef }
	w["getComputedStyle"] = func(a ...any) any {
		// 真页面返回 CSSStyleDeclaration；挑战程序通常只读少数属性
		return map[string]any{"display": "block", "visibility": "visible", "opacity": "1",
			"getPropertyValue": func(b ...any) any { return "" }}
	}
	w["matchMedia"] = func(a ...any) any {
		return map[string]any{"matches": false, "media": jsStr(arg(a, 0)),
			"addListener": func(b ...any) any { return undef }, "removeListener": func(b ...any) any { return undef },
			"addEventListener": func(b ...any) any { return undef }, "removeEventListener": func(b ...any) any { return undef }}
	}
	w["addEventListener"] = func(a ...any) any { return undef }
	w["removeEventListener"] = func(a ...any) any { return undef }
	w["dispatchEvent"] = func(a ...any) any { return true }
	w["crypto"] = map[string]any{
		"getRandomValues": func(a ...any) any {
			if arr, ok := arg(a, 0).([]any); ok {
				for i := range arr {
					arr[i] = float64(rand.Intn(256))
				}
			}
			return arg(a, 0)
		},
		"randomUUID": func(a ...any) any { return newUUID() },
	}
	return w
}

// newUUID 生成 v4 UUID 字符串（与 C 的 new_uuid 同形）
func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newElement 构造一个最小的 DOM 元素（挑战程序主要用它取 WebGL 指纹）
func newElement(tag string, e Env) any {
	el := map[string]any{
		"tagName":     strings.ToUpper(tag),
		"nodeName":    strings.ToUpper(tag),
		"nodeType":    float64(1),
		"style":       map[string]any{"cssText": "", "display": "block", "visibility": "visible"},
		"dataset":     map[string]any{},
		"children":    []any{},
		"childNodes":  []any{},
		"classList":   map[string]any{"add": func(a ...any) any { return undef }, "remove": func(a ...any) any { return undef }, "contains": func(a ...any) any { return false }},
		"attributes":  map[string]any{},
		"innerHTML":   "",
		"textContent": "",
		"width":       float64(300),
		"height":      float64(150),
		"getContext":  func(a ...any) any { return newGLContext(e) },
		"getBoundingClientRect": func(a ...any) any {
			return map[string]any{"x": float64(0), "y": float64(0), "width": float64(300), "height": float64(150),
				"top": float64(0), "left": float64(0), "right": float64(300), "bottom": float64(150)}
		},
		"appendChild":         func(a ...any) any { return arg(a, 0) },
		"removeChild":         func(a ...any) any { return arg(a, 0) },
		"setAttribute":        func(a ...any) any { return undef },
		"getAttribute":        func(a ...any) any { return nil },
		"addEventListener":    func(a ...any) any { return undef },
		"removeEventListener": func(a ...any) any { return undef },
		"toDataURL":           func(a ...any) any { return "data:image/png;base64,iVBORw0KGgo=" },
	}
	return el
}

// newGLContext 构造 WebGL 上下文桩：只返回挑战程序会读的几个常量与字符串
func newGLContext(e Env) any {
	gl := map[string]any{
		"VENDOR": float64(0x1F00), "RENDERER": float64(0x1F01), "VERSION": float64(0x1F02),
		"UNMASKED_VENDOR_WEBGL":   float64(0x9245),
		"UNMASKED_RENDERER_WEBGL": float64(0x9246),
		"MAX_TEXTURE_SIZE":        float64(0x0D33),
		"getParameter": func(a ...any) any {
			switch int(jsNum(arg(a, 0))) {
			case 0x1F00, 0x9245:
				return "Google Inc. (Intel)"
			case 0x1F01, 0x9246:
				return "ANGLE (Intel, Intel(R) UHD Graphics 630 Direct3D11 vs_5_0 ps_5_0, D3D11)"
			case 0x1F02:
				return "WebGL 1.0 (OpenGL ES 2.0 Chromium)"
			case 0x0D33:
				return float64(16384)
			}
			return nil
		},
		"getExtension":           func(a ...any) any { return map[string]any{} },
		"getSupportedExtensions": func(a ...any) any { return []any{} },
		"getShaderPrecisionFormat": func(a ...any) any {
			return map[string]any{"precision": float64(23), "rangeMin": float64(127), "rangeMax": float64(127)}
		},
	}
	return gl
}
