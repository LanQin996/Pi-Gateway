package admin

// Shared wire values and diagnostic messages.
const (
	gatewayAccountsFormat           = "pi-gateway-accounts"
	gatewayFormat                   = "pi-gateway"
	sub2apiDataFormat               = "sub2api-data"
	sub2apiBundleFormat             = "sub2api-bundle"
	accountNotFoundMessage          = "account not found"
	phaseFieldPath                  = "/phase"
	captureIDFieldPath              = "/capture_id"
	batchTokenFieldPath             = "/batch_token"
	expectedRevisionRequiredMessage = "expected_revision is required"
	invalidAccountIDMessage         = "invalid account id"
	cachedWebSocketTransport        = "websocket-cached"
	proxyURLPrefix                  = "proxy_url: "
	keyNotFoundMessage              = "key not found"
	invalidProxyPayloadMessage      = "invalid proxy payload"
	codexCredentialRequiredMessage  = "this account has no Codex credential; link one to read quota"
)
