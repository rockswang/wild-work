package traework

import "sync"

// 模型 → function 的实际归属注册表（TraeWork / TraeCode 渠道共享）。
//
// 背景：chat 接口 llm_utils_chat 的 function 参数决定模型池，而 get_detail_param
// 的模型目录不保证按 function 过滤（实测会一并下发多个池的模型）。只按本渠道
// function 拉目录/发请求，会出现「/v1/models 里列得出、一调用就 4001
// param is invalid」的错位——上游对用错池的请求不报「模型不存在」。
//
// 故：
//   - FetchModels 双池合并时，把每个模型记入它实际所在的池（先到的池赢）；
//   - PrepareBody 发请求前按模型名反查归属池，覆盖渠道默认 function。
//
// map 为包级共享：两个 function 是同一上游、共享账号池，模型在哪个池是客观事实，
// 与渠道实例无关。RWMutex 保护并发刷新（多渠道、定时任务与请求并发）。
var (
	modelFuncMu sync.RWMutex
	modelFunc   = map[string]string{
		// 内置兜底：即使模型目录尚未拉取，已知 solo_agent 池专属模型也按其归属池调用
		"qwen3.8-flash": FunctionCode,
		"glm-5.3-flash": FunctionCode,
	}
)

// recordModelFunc 登记模型归属（只增不改：后见条目不覆盖先见，与 FetchModels
// 的先到先得去重同序——先到的池赢）。错误的归属会导致 4001，宁缺毋滥。
func recordModelFunc(model, function string) {
	if model == "" || function == "" {
		return
	}
	modelFuncMu.Lock()
	defer modelFuncMu.Unlock()
	if _, ok := modelFunc[model]; !ok {
		modelFunc[model] = function
	}
}

// functionForModel 反查模型归属池；未登记返回空串，调用方沿用渠道默认 function。
func functionForModel(model string) string {
	modelFuncMu.RLock()
	defer modelFuncMu.RUnlock()
	return modelFunc[model]
}

// otherFunction 返回另一池的 function（双池合并用）。
func otherFunction(function string) string {
	if function == Function {
		return FunctionCode
	}
	return Function
}
