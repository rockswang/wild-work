// 事件口径记账：直读本进程日志文件（data/app.log），解析签到成功事件并幂等累计
// 今日收入积分，替代差值的收入口径——「登录即自动签到」等场景账号条目晚于入账
// 建立/被删重建，余额差值恒 0 漏记，故收入必须走日志事件。
//
// 行格式（本仓源码逐行核实；两种格式并存，解析双向兼容）：
//
//	scheduler.go:429    checkin result platform=%s uid=%s ok=%t retryable=%t msg=%s remain=%d has_remain=%t
//	app.go:2074         checkin platform=%s uid=%s ok=%t msg=%s remain=%d has_remain=%t
//	app.go（无平台）     checkin uid=%s ok=%t ...（无平台无法定价，忽略）
//	traework/client.go:414  traework checkin status uid=%s checked_in=%t credits=%d enable=%t
//	traework/client.go:455  traework checkin claim response uid=%s code=%d msg=%s
//	qodercn/checkin.go:81、qodercom/checkin.go:77（结构化上报）：
//	      <p> checkin campaigns uid=%s status=%s claimed=%t amount=%d msg=%s err=%v
//	      status ∈ claimed/already_claimed/no_campaign/no_token/error，仅 claimed 有新入账
//	raccoon/checkin.go（结构化上报，R65，与 qoder 同形状）：
//	      raccoon checkin setting_info uid=%s status=%s claimed=%t amount=%d msg=%s err=%v
//	      status ∈ claimed/already_claimed/no_token/error，仅 claimed 有新入账
//	旧版日志兼容（历史 app.log 行仍可能存在）：
//	      <p> checkin (daily|campaigns) uid=%s ok=%t claimed=%t amount=%d msg=%s
//
// 数额：workbuddy 上游丢弃签到响应体，观测恒 100/日 → 默认表；traework 默认 100，
// 以同日 status 行 credits 精确修正（双向，官方再调数额自动跟随）；qoder 系与
// 小浣熊取日志 amount（上游回执的实发数额）；其余平台无数额来源不记。
// 「已签到」「活跃保活」无新入账，跳过。
package stats

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

// platformDefaultAmount 统一成功行的平台默认数额（日志无数额字段时的兜底，
// 待同日 status 行精确修正）。
var platformDefaultAmount = map[string]int64{
	"workbuddy": 100,
	"traework":  100,
}

var (
	// 统一行：checkin [result] [platform=P] uid=U ok=B [retryable=B] msg=...（msg 值惰性
	// 截到 " remain="；失败行 msg 内嵌 " msg=" 无所谓，ok=false 不采信；retryable= 为
	// 签到窗口重试的可选字段，缺失时匹配旧行）。
	reCheckinUnified = regexp.MustCompile(
		`^(\d{4})/(\d{2})/(\d{2}) \d{2}:\d{2}:\d{2}\.\d+ checkin (?:result )?(?:platform=(\S+) )?uid=(\S+) ok=(true|false) (?:retryable=(?:true|false) )?msg=(.*?)(?: remain=\d+ has_remain=\S+)?$`)
	// TW 领取结果（登录自动签到路径只走这行）：code=0 即成功。
	reTWClaim = regexp.MustCompile(
		`^(\d{4})/(\d{2})/(\d{2}) \d{2}:\d{2}:\d{2}\.\d+ traework checkin claim response uid=(\S+) code=(\d+) `)
	// TW 状态行：credits= 当日奖励数额（checked_in=true 时用于修正已记事件数额）。
	reTWStatus = regexp.MustCompile(
		`^(\d{4})/(\d{2})/(\d{2}) \d{2}:\d{2}:\d{2}\.\d+ traework checkin status uid=(\S+) checked_in=(true|false) credits=(\d+) enable=`)
	// qoder 系 + 小浣熊领取行（旧格式，历史日志兼容）：daily/campaigns 二选一，ok 且
	// claimed=true 才有新入账。
	reQoderClaim = regexp.MustCompile(
		`^(\d{4})/(\d{2})/(\d{2}) \d{2}:\d{2}:\d{2}\.\d+ (qodercn|qodercom) checkin (?:daily|campaigns) uid=(\S+) ok=(true|false) claimed=(true|false) amount=(\d+) `)
	// 结构化领取行（qoder 系与 raccoon 共用形状）：ok= 改 status=（claimed/
	// already_claimed/no_campaign/no_token/error），窗口内每分钟重试会重复落行，
	// uid|day 幂等去重兜底。小浣熊（R65）的日志行与 qoder 同构
	// （`raccoon checkin setting_info uid=… status=… claimed=… amount=…`），
	// 语义也一致（结构化 status + 上游实发 amount），故共用本条正则。
	reQoderClaimStatus = regexp.MustCompile(
		`^(\d{4})/(\d{2})/(\d{2}) \d{2}:\d{2}:\d{2}\.\d+ (qodercn|qodercom|raccoon|officeace) checkin (?:daily|campaigns|setting_info|bonus) uid=(\S+) status=(\S+) claimed=(true|false) amount=(\d+) `)
	// reCheckinShape 松形状探针（只服务「解析失效告警」，不参与记账）：只认上列 5 种
	// **结果类**行的开头形状，不约束后续字段。上游改字段名/增删字段时严格正则会全数
	// 失配（收入统计静默归零，比面板失效更隐蔽），而本探针仍会命中——两者一比即知
	// 解析器已跟不上日志文案。刻意**不**匹配 start/failed/batch/credits/token invalid
	// 等过程行（它们本就不记账，纳入会常态误报）。
	reCheckinShape = regexp.MustCompile(
		`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d+ (?:checkin (?:result |platform=|uid=)|traework checkin (?:status|claim response) uid=|(?:qodercn|qodercom|raccoon|officeace) checkin (?:daily|campaigns|setting_info|bonus) uid=)`)
)

// RescanEventLog 全量重扫本进程日志文件并喂给事件解析器（幂等，可反复调用）。
// 全量重扫而非增量跟踪：文件按日增长（千行级/日），周期读入成本可忽略，
// 换来零游标状态、重启/日志重建后自动收敛。比 /api/logs 的固定行数窗口可靠。
// 顺带做解析健康度自检：形状像签到结果但严格正则全不命中 → 告警（见 noteEventScan）。
func (s *Stats) RescanEventLog(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logf("事件日志读取失败 %s: %v", path, err)
		}
		return
	}
	var strict, unparsed int
	sample := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if s.observeLine(line) {
			strict++
			continue
		}
		if reCheckinShape.MatchString(line) {
			unparsed++
			if sample == "" {
				sample = line
			}
		}
	}
	s.noteEventScan(strict, unparsed, sample)
}

// ObserveLogLine 解析单行日志，命中签到事件则记账（转发路径实时喂入可选；
// 全量重扫已覆盖，此处仅保留入口）。
func (s *Stats) ObserveLogLine(line string) { s.observeLine(line) }

// observeLine 单行解析；返回值 = **格式是否被严格正则识别**（与是否记账无关：
// ok=false / 无平台 / 非今日的行同样算「已识别」，只有格式对不上才是解析失效）。
func (s *Stats) observeLine(line string) bool {
	line = strings.TrimRight(line, "\r\n \t")
	if m := reCheckinUnified.FindStringSubmatch(line); m != nil {
		if m[6] != "true" {
			return true
		}
		switch strings.TrimSpace(m[7]) {
		case "已签到", "活跃保活": // 良性重复签到 / 无积分保活：无新入账
			return true
		}
		amount, ok := platformDefaultAmount[m[4]]
		if !ok {
			return true // 无平台行（无法定价）或未知平台：跳过
		}
		s.recordEvent(m[4], m[5], eventDay(m), amount, true)
		return true
	}
	if m := reTWClaim.FindStringSubmatch(line); m != nil {
		if m[5] != "0" {
			return true
		}
		s.recordEvent("traework", m[4], eventDay(m), platformDefaultAmount["traework"], true)
		return true
	}
	if m := reTWStatus.FindStringSubmatch(line); m != nil {
		if m[5] != "true" {
			return true
		}
		amount, _ := strconv.ParseInt(m[6], 10, 64)
		s.recordEvent("traework", m[4], eventDay(m), amount, false) // 仅修正，不新建
		return true
	}
	if m := reQoderClaim.FindStringSubmatch(line); m != nil {
		if m[6] != "true" || m[7] != "true" {
			return true
		}
		amount, _ := strconv.ParseInt(m[8], 10, 64)
		s.recordEvent(m[4], m[5], eventDay(m), amount, true)
		return true
	}
	if m := reQoderClaimStatus.FindStringSubmatch(line); m != nil {
		// 结构化行：仅 status=claimed 是新入账；already_claimed/no_campaign/
		// no_token/error 均无收入。claimed 旗标做双重校验（上游同义下发）。
		if m[6] != "claimed" || m[7] != "true" {
			return true
		}
		amount, _ := strconv.ParseInt(m[8], 10, 64)
		s.recordEvent(m[4], m[5], eventDay(m), amount, true)
		return true
	}
	return false
}

// noteEventScan 记录一轮重扫的解析健康度。降级判据 = **有签到结果形状的行、却一行
// 都没被严格正则识别**（= 日志文案已变，收入统计已归零）。日志按边沿触发
// （周期重扫不节流会刷屏）：进入降级报一次、恢复报一次。
func (s *Stats) noteEventScan(strict, unparsed int, sample string) {
	degraded := strict == 0 && unparsed > 0
	if degraded {
		sample = clipRunes(sample, 160)
	} else {
		sample = ""
	}
	s.mu.Lock()
	s.evStrict, s.evUnparsed, s.evSample = strict, unparsed, sample
	changed := degraded != s.evDegraded
	s.evDegraded = degraded
	s.mu.Unlock()
	if !changed {
		return
	}
	if degraded {
		s.logf("告警: 签到日志解析失效——%d 行签到结果行无一命中严格正则（今日收入统计已归零，需按新文案改正则）样本: %s",
			unparsed, sample)
		return
	}
	s.logf("签到日志解析恢复（本轮命中 %d 行）", strict)
}

// clipRunes 按字符（非字节）截断，避免切断 UTF-8 多字节字符产生乱码。
func clipRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// recordEvent 记一笔签到事件收入（按事件行首日期归日）。create=true 时事件不
// 存在则新建；false 时按 status 行 credits **精确落值**（可上可下：默认表兜底值
// 与官方实发不一致时双向修正——只上调不修正会在官方调低数额时虚记）。
// 去重键 uid|day：每账号每日至多一次成功入账，统一行/claim 行/重复日志形态
// 天然去重；非今日的事件行直接丢弃，跨天重扫不会误记。
func (s *Stats) recordEvent(platform, uid, day string, amount int64, create bool) {
	if platform == "" || uid == "" || amount < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if day != s.today {
		return
	}
	key := uid + "|" + day
	if old, ok := s.events[key]; ok {
		if create && amount > old {
			// create 行无数额依据（默认表兜底），仅在大于已记值时上调
			s.creditInTotal += amount - old
			s.events[key] = amount
			s.dirty = true
		} else if !create && amount != old {
			// status 行带官方实发数额，精确修正（双向）
			s.creditInTotal += amount - old
			s.events[key] = amount
			s.dirty = true
		}
		return
	}
	if !create {
		return
	}
	s.events[key] = amount
	s.creditInTotal += amount
	s.dirty = true
}

// eventDay 从行首时间戳捕获组取 "YYYY-MM-DD"（与 todayKey 同构，可直接比较）。
func eventDay(m []string) string {
	return m[1] + "-" + m[2] + "-" + m[3]
}
