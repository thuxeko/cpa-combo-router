package main

import (
	"context"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	pluginID   = "combo-router"
	pluginName = "Combo Router"
)

// pluginVersion is what the metadata reports. Note that CPA derives the plugin
// version it logs from the .so FILE NAME, so the file must be named
// combo-router-<version>.so to match.
var pluginVersion = "1.0.0"

// comboRouterPlugin keeps only what routing needs: the parsed configuration and
// the live cooldown/circuit state. The usage, pricing and dashboard stores of
// 0.5.4 were removed on purpose — the separate usage-statistics plugin already
// records alias traffic, and a second SQLite writer on a two-core host costs
// more than it explains.
type comboRouterPlugin struct {
	config  routerConfig
	runtime *routeRuntime
}

var (
	_ pluginapi.ModelRouter      = (*comboRouterPlugin)(nil)
	_ pluginapi.ModelRegistrar   = (*comboRouterPlugin)(nil)
	_ pluginapi.ProviderExecutor = (*comboRouterPlugin)(nil)
)

func newComboRouterPlugin(configYAML []byte, previous *comboRouterPlugin) (*comboRouterPlugin, pluginapi.Metadata, error) {
	cfg, err := decodeRouterConfig(configYAML)
	if err != nil {
		return nil, pluginapi.Metadata{}, err
	}
	var runtime *routeRuntime
	if previous != nil {
		// A reconfigure keeps the cooldowns of every route whose shape did not
		// change, so editing one route does not clear another's circuit.
		runtime = previous.runtime.Clone(cfg.Routes)
	} else {
		runtime = newRouteRuntime(nil)
		runtime.Sync(cfg.Routes)
		// Restore cooldowns written by the previous process, so a restart does
		// not immediately send traffic back to a target that was failing.
		runtime.Load(cfg.StatePath)
	}
	// Load only adopts the path when the file already exists, so on a first run
	// persistPath would stay empty and no cooldown would ever be written. Set it
	// explicitly; an empty StatePath still means "do not persist".
	runtime.SetPersistPath(cfg.StatePath)
	// Live traffic gets jitter; tests build the runtime directly and stay
	// deterministic.
	runtime.EnableJitter()
	// The per-provider in-flight budget belongs to the upstream account, so it
	// is applied after Clone: editing a route must not silently drop it.
	runtime.SetProviderLimits(cfg.ProviderLimits)
	// The degraded window drives both the health-aware ordering and the
	// shortened first-byte timeout for targets that failed recently.
	runtime.SetHealthWindow(time.Duration(cfg.HealthWindowSeconds) * time.Second)
	plugin := &comboRouterPlugin{config: cfg, runtime: runtime}
	metadata := pluginapi.Metadata{
		Name:             pluginName,
		Version:          pluginVersion,
		Author:           "thuxeko",
		GitHubRepository: "https://github.com/thuxeko/cpa-combo-router",
		ConfigFields: []pluginapi.ConfigField{
			{Name: "routes", Type: pluginapi.ConfigFieldTypeArray, Description: "Các tuyến (alias) logic — mỗi tuyến là một nhóm đích dự phòng chạy theo thứ tự ưu tiên hoặc phân tải theo trọng số. Ví dụ: tuyến \"balanced\" gồm 3 đích ca2/glm-5.3, a6/glm-5.3, vsl/glm-5.3 — đích đầu lỗi hoặc đang nguội thì tự chuyển sang đích kế tiếp."},
			{Name: "error_policy", Type: pluginapi.ConfigFieldTypeObject, Description: "Chính sách xử lý theo từng mã lỗi HTTP: action (fallback = chuyển đích kế, stop = dừng và trả lỗi, fallback_no_penalty = chuyển đích nhưng không phạt), cooldown_seconds (thời gian nguội), max_cooldown_seconds (trần nguội), honor_retry_after (tuân theo Retry-After của provider), backoff (tăng dần thời gian nguội). Ví dụ: {\"503\": {\"action\": \"fallback\", \"cooldown_seconds\": 30}}."},
			{Name: "attempt_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Số giây tối đa chờ byte đầu tiên từ MỘT đích trước khi router bỏ và thử đích kế tiếp (0 = không giới hạn). Ví dụ: 45 — model lớn thường mất 30–40s mới có byte đầu, nên để dưới ngưỡng này sẽ cắt oan."},
			{Name: "provider_limits", Type: pluginapi.ConfigFieldTypeObject, Description: "Giới hạn số request đồng thời (in-flight) cho mỗi provider, dùng chung giữa mọi tuyến chạm tới nó. Ví dụ: {\"ca2\": 5} — request thứ 6 vào ca2 sẽ không gửi lên provider mà rơi thẳng sang đích kế tiếp, tránh bị provider chặn vì quá tải."},
			{Name: "attempt_retries", Type: pluginapi.ConfigFieldTypeInteger, Description: "Số lần thử lại NGAY TẠI CÙNG một đích trước khi chuyển sang đích kế, ví dụ 3. Provider trả lời \"bận\" trong vài giây chưa hẳn là đích chết — thử lại giúp tránh phạt nguội oan cả tuyến đang khỏe. 0 (mặc định) = mỗi đích chỉ thử 1 lần."},
			{Name: "retry_backoff_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Số giây nghỉ giữa 2 lần thử lại cùng một đích (mặc định 2). Nếu provider gửi kèm header Retry-After lớn hơn thì dùng giá trị của provider."},
			{Name: "retry_deadline_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Tổng thời gian (giây) một request được phép dành cho việc thử lại trước khi bỏ cuộc (mặc định 90). Phải nhỏ hơn timeout của chính client (IDE, curl…) — nếu không client sẽ tự ngắt trước và log chỉ còn 499 khó truy nguyên nhân."},
			{Name: "health_window_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Thời gian (giây) một đích bị \"hạ sức khỏe\" sau lần lỗi gần nhất: bị đẩy xuống cuối thứ tự ưu tiên và bị áp timeout byte-đầu ngắn hơn (0 = tắt tính năng; mặc định 300). Ví dụ: đích vừa lỗi 502 sẽ bị tránh trong 5 phút, hết cửa sổ thì tự phục hồi vị trí."},
			{Name: "degraded_attempt_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Timeout chờ byte đầu (giây) áp riêng cho đích đang bị hạ sức khỏe — ngắn hơn attempt_timeout_seconds để một đích chậm ít làm mất thời gian hơn (0 = dùng chung attempt_timeout_seconds). Ví dụ: 15 khi attempt_timeout_seconds = 45."},
			{Name: "max_cooldown_wait_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Khi MỌI đích của tuyến đều đang nguội (cooldown), request sẽ chờ tối đa số giây này cho đích sớm tỉnh lại rồi chạy tiếp, thay vì trả ngay lỗi 503 \"no available candidate\" (mặc định 20, tối đa 60; 0 = tắt chờ, giữ hành vi cũ trả lỗi ngay). Ví dụ: tuyến balanced có cooldown 30s — đặt 20 nghĩa là IDE chờ thêm tối đa 20s là gần như luôn bắt được đích vừa tỉnh."},
			{Name: "fallback", Type: pluginapi.ConfigFieldTypeObject, Description: "Danh sách mã lỗi kiểu cũ, giữ lại để tương thích cấu hình trước đây: fallback_on_status (các mã được chuyển đích), no_fallback_on_status (các mã không được chuyển đích). Cấu hình mới nên dùng error_policy."},
			{Name: "state_path", Type: pluginapi.ConfigFieldTypeString, Description: "Đường dẫn file lưu trạng thái nguội (cooldown) để giữ nguyên sau khi khởi động lại CPA; mặc định là combo-router-state.json trong thư mục plugins của CPA."},
		},
	}
	return plugin, metadata, nil
}

func (p *comboRouterPlugin) Identifier() string { return pluginID }

func (p *comboRouterPlugin) Execute(_ context.Context, _ pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, fmt.Errorf("%s execution requires the native host callback context", pluginID)
}

func (p *comboRouterPlugin) ExecuteStream(_ context.Context, _ pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	return pluginapi.ExecutorStreamResponse{}, fmt.Errorf("%s streaming requires the native stream bridge", pluginID)
}

func (p *comboRouterPlugin) CountTokens(_ context.Context, _ pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, newRouteError(501, "model_route_count_tokens_unsupported", "", "token counting for routed aliases is not supported by the current CLIProxyAPI plugin ABI", "")
}

func (p *comboRouterPlugin) HttpRequest(_ context.Context, _ pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return pluginapi.ExecutorHTTPResponse{}, fmt.Errorf("%s does not implement executor.http_request", pluginID)
}

func main() {}
