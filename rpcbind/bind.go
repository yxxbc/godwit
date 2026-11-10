// Package rpcbind composes core with rpc: core's error codes, redaction and
// the methods exposed over the Local API. core does not depend on rpc.
//
// Its error table is the only place an error's wire code and i18n key are
// written; the native UI and the pages show errors by Key.
package rpcbind

import (
	"context"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/ai"
	"github.com/godwit-email/godwit/core/auth"
	"github.com/godwit-email/godwit/core/conn"
	"github.com/godwit-email/godwit/core/plugins"
	"github.com/godwit-email/godwit/rpc"
)

// table is the one list of an error's identity: core's exported errors (with
// those of core/ai, core/auth and core/conn that its methods return, and the
// context ones) by stable wire code, and the i18n key of what each means to the
// user, "error.<code>" unless Key says otherwise. Their messages are fixed
// English text. A row without a code has a message but does not cross the
// Local API; the first row an error is wins.
var table = []Error{
	// A native plugin's failures: its stderr is the plugin's text, not the user's.
	{Err: plugins.ErrNativeExited, Key: "error.plugin_exited"},
	{Err: plugins.ErrTimeout, Key: "error.plugin_timeout"},
	{Err: plugins.ErrNativeABI, Key: "error.plugin_abi"},
	{Err: auth.ErrRedirectPortBusy, Key: "settings.models.login.port_busy"},
	{Code: "use_assist", Err: core.ErrUseAssist},
	{Code: "stale_approval", Err: core.ErrStaleApproval},
	{Code: "too_large", Err: core.ErrTooLarge},
	{Code: "link_ref", Err: core.ErrLinkRef},
	{Code: "placeholder", Err: core.ErrPlaceholder},
	{Code: "mask_kind", Err: core.ErrMaskKind},
	{Code: "mask_reserved_kind", Err: core.ErrMaskReservedKind},
	{Code: "mask_empty_match", Err: core.ErrMaskEmptyMatch},
	{Code: "mask_too_many_rules", Err: core.ErrMaskTooManyRules},
	{Code: "mask_too_many_allow", Err: core.ErrMaskTooManyAllow},
	{Code: "not_on_server_yet", Err: core.ErrNotOnServerYet},
	{Code: "stale_draft", Err: core.ErrStaleDraft},
	{Code: "not_synced", Err: core.ErrNotSynced},
	{Code: "run_active", Err: core.ErrRunActive},
	{Code: "security_scan_busy", Err: core.ErrSecurityScanBusy, Key: "settings.security.rescan.busy"},
	{Code: "security_scan_disabled", Err: core.ErrSecurityScanDisabled},
	{Code: "compacting", Err: core.ErrCompacting},
	{Code: "nothing_to_compact", Err: core.ErrNothingToCompact},
	{Code: "draft_gone", Err: core.ErrDraftGone, Key: "wit.draft.gone"},
	{Code: "keychain_unavailable", Err: core.ErrKeychainUnavailable, Key: "error.keychain"},
	{Code: "wrong_account", Err: core.ErrWrongAccount},
	{Code: "account_not_found", Err: core.ErrAccountNotFound},
	{Code: "local_archive", Err: core.ErrLocalArchive},
	{Code: "not_local_archive", Err: core.ErrNotLocalArchive},
	{Code: "receive_only", Err: core.ErrReceiveOnly},
	{Code: "role_folder", Err: core.ErrRoleFolder},
	{Code: "folder_not_empty", Err: core.ErrFolderNotEmpty},
	{Code: "folder_name_empty", Err: core.ErrFolderNameEmpty},
	{Code: "folder_exists", Err: core.ErrFolderExists},
	{Code: "provider_mismatch", Err: core.ErrProviderMismatch},
	{Code: "needs_grant", Err: core.ErrNeedsGrant},
	{Code: "plugin_not_found", Err: core.ErrPluginNotFound},
	{Code: "translator_unavailable", Err: core.ErrTranslatorUnavailable},
	{Code: "system_language_not_installed", Err: core.ErrSystemLanguageNotInstalled},
	{Code: "system_language_unsupported", Err: core.ErrSystemLanguageUnsupported},
	{Code: "model_unavailable", Err: core.ErrModelUnavailable},
	{Code: "registry_off", Err: core.ErrRegistryOff},
	{Code: "registry_signature", Err: core.ErrRegistrySignature},
	{Code: "registry_stale", Err: core.ErrRegistryStale},
	{Code: "registry_mismatch", Err: core.ErrRegistryMismatch},
	{Code: "recognition_model_missing", Err: core.ErrRecognitionModelMissing},
	{Code: "recognition_model_mismatch", Err: core.ErrRecognitionModelMismatch},
	{Code: "mcp_not_found", Err: core.ErrMCPNotFound},
	{Code: "ai_not_configured", Err: core.ErrAINotConfigured},
	{Code: "model_service_not_found", Err: core.ErrModelServiceNotFound},
	{Code: "model_not_found", Err: core.ErrModelNotFound},
	{Code: "model_auth", Err: core.ErrModelAuth},
	{Code: "model_login", Err: core.ErrModelLogin},
	{Code: "model_reauth", Err: core.ErrModelReauth, Key: "settings.models.login.expired"},
	{Code: "label_exists", Err: core.ErrLabelExists},
	{Code: "base_url_invalid", Err: core.ErrBaseURLInvalid},
	{Code: "base_url_scheme", Err: core.ErrBaseURLScheme},
	{Code: "no_model_list", Err: ai.ErrNoModelList},
	{Code: "model_forbidden", Err: ai.ErrModelForbidden},
	{Code: "key_needs_confirm", Err: core.ErrKeyNeedsConfirm, Key: "settings.models.key_moved"},
	{Code: "unknown_flow", Err: core.ErrUnknownFlow},
	{Code: "invalid_state", Err: core.ErrInvalidState},
	{Code: "skill_disabled", Err: core.ErrSkillDisabled},
	{Code: "skill_not_found", Err: core.ErrSkillNotFound},
	{Code: "closed", Err: core.ErrClosed},
	{Code: "signature_image", Err: core.ErrSignatureImage},
	{Code: "signature_image_count", Err: core.ErrSignatureImageCount},
	{Code: "signature_not_found", Err: core.ErrSignatureNotFound},
	{Code: "signature_name", Err: core.ErrSignatureName},
	{Code: "invalid_rule", Err: core.ErrInvalidRule},
	{Code: "invalid_view", Err: core.ErrInvalidView},
	{Code: "recipient_unconfirmed", Err: core.ErrRecipientUnconfirmed},
	{Code: "account_icon", Err: core.ErrAccountIcon},
	{Code: "not_grantable", Err: core.ErrNotGrantable},
	{Code: "needs_answer", Err: core.ErrNeedsAnswer},
	{Code: "mcp_send_off", Err: core.ErrMCPSendOff},
	{Code: "mcp_send_composing", Err: core.ErrMCPSendComposing},
	{Code: "mcp_send_busy", Err: core.ErrMCPSendBusy},
	{Code: "no_outbox_draft", Err: core.ErrNoOutboxDraft},
	{Code: "no_unsubscribe", Err: core.ErrNoUnsubscribe},
	{Code: "unsubscribe_web", Err: core.ErrUnsubscribeWeb},
	{Code: "unsubscribing", Err: core.ErrUnsubscribing},
	{Code: "purge_too_late", Err: core.ErrPurgeTooLate, Key: "mail.purge.too_late"},
	{Code: "canceled", Err: context.Canceled},
	{Code: "deadline", Err: context.DeadlineExceeded},
	{Code: "auth_failed", Err: conn.ErrAuthFailed, Key: "settings.account.detail.auth_rejected"}, // also a failed account check's cause; the account's own words for it
	{Code: "oauth_not_configured", Err: auth.ErrOAuthNotConfigured},
	{Code: "flow_expired", Err: auth.ErrFlowExpired},
	{Code: "invalid_grant", Err: auth.ErrInvalidGrant},
	{Code: "access_denied", Err: auth.ErrAccessDenied},
	{Code: "scope_missing", Err: auth.ErrScopeMissing},
}

// Error is a row of the table.
type Error struct {
	Code string // "" when it does not cross the Local API
	Err  error
	Key  string // "" for "error." + Code
}

// All is every row of the table.
func All() []Error { return table }

// Key is the i18n key of what err means to the user, by the first row it is;
// "" when it is none. The key may have no message in the catalog yet.
func Key(err error) string {
	for _, e := range table {
		if errors.Is(err, e.Err) {
			if e.Key != "" {
				return e.Key
			}
			return "error." + e.Code
		}
	}
	return ""
}

// NewCodec registers core's error types (tried first, so *ai.Error with Code
// canceled stays an *ai.Error) and sentinels.
func NewCodec() *rpc.Codec {
	c := &rpc.Codec{}
	// The first failing step's error (Unwrap) stays on the server; only the
	// sentinel it is crosses, as the cause.
	rpc.RegisterType(c, "check_failed",
		func(e *core.CheckError) any { return e.Report },
		func(raw json.RawMessage) (*core.CheckError, error) {
			r, err := rpc.DecodeDetail[core.CheckReport](raw)
			if err != nil {
				return nil, err
			}
			return &core.CheckError{Report: *r}, nil
		})
	rpc.RegisterType(c, "send_warnings",
		func(e *core.SendWarningsError) any { return e.Check },
		func(raw json.RawMessage) (*core.SendWarningsError, error) {
			ch, err := rpc.DecodeDetail[core.SendCheck](raw)
			if err != nil {
				return nil, err
			}
			return &core.SendWarningsError{Check: *ch}, nil
		})
	rpc.RegisterType(c, "send_blocked",
		func(e *core.SendBlockedError) any { return core.SendBlockedError{Plugin: e.Plugin, Message: e.Message} },
		rpc.DecodeDetail[core.SendBlockedError])
	rpc.RegisterType(c, "egress_approval_required",
		func(e *core.EgressApprovalRequired) any {
			return core.EgressApprovalRequired{Plugin: e.Plugin, Hosts: e.Hosts}
		},
		rpc.DecodeDetail[core.EgressApprovalRequired])
	rpc.RegisterType(c, "no_role_folder",
		func(e *core.ErrNoRoleFolder) any { return *e },
		rpc.DecodeDetail[core.ErrNoRoleFolder])
	rpc.RegisterType(c, "model_error",
		func(e *core.ModelError) any {
			return core.ModelError{Code: e.Code, Message: e.Message, Retryable: e.Retryable}
		},
		rpc.DecodeDetail[core.ModelError])
	rpc.RegisterType(c, "ai_error",
		func(e *ai.Error) any { return ai.Error{Code: e.Code, Msg: e.Msg, Detail: e.Detail} },
		rpc.DecodeDetail[ai.Error])
	for _, e := range table {
		if e.Code != "" {
			c.RegisterSentinel(e.Code, e.Err, e.Err.Error())
		}
	}
	return c
}

// hidden are the exported *core.Service methods kept off the Local API, each
// for its reason. full (ADR 0002) gets every other exported method, so a new
// one is exposed by default: testdata/full_methods.txt pins the result, and a
// method that must not reach the user's programs goes here instead.
var hidden = map[string]bool{
	"Close":         true, // the owner's lifetime, not a client's
	"Redact":        true, // a client could test guesses of the owner's secrets against it
	"AuditExternal": true, // writes audit rows; only the owner records what clients did
	"Proxy":         true, // a custom proxy's URL carries its password
	// They fetch from remote hosts for the native UI.
	"SenderAvatar": true, "ModelServiceAvatar": true, "PluginReadme": true,
	// A channel and a stop func; RPC clients have SubscribeConversation.
	"FollowConversation": true,
	// A progress callback cannot cross the wire; the first two have forms without one.
	"InstallPluginFromRegistryWithProgress": true, "PrepareModelWithProgress": true, "DownloadRecognitionModel": true,
	// The native reader's two steps of GetMessage, which clients have whole; a callback too.
	"GetMessageLazyImages": true, "InlineImages": true,
	// The GUI's window state, not a client's; a sync a client asks for is SyncNow, whole.
	"SetLightBackground": true, "LightBackground": true,
}

// Expose picks the *core.Service methods callable over RPC: every exported
// one but hidden and the test hooks (…ForTest).
func Expose(method string) bool {
	return !hidden[method] && !strings.HasSuffix(method, "ForTest")
}

// Listen serves svc's exposed methods on <dataDir>/godwit.sock (see
// rpc.Listen; the caller holds the owner lock). Errors go out through
// NewCodec; the API token and svc's secrets are redacted from them and from
// the log. Sends and forwarding rules by the full role and drafts by the mcp
// role are audited before they are dispatched.
func Listen(dataDir string, svc *core.Service, version string) (*rpc.Server, error) {
	d, err := rpc.NewDispatcher(svc, Expose)
	if err != nil {
		return nil, err
	}
	return rpc.Listen(dataDir, d, rpc.ServerOptions{
		Version: version,
		Codec:   NewCodec(),
		Redact:  svc.Redact,
		Audit:   func(role, method string, params []json.RawMessage) error { return audit(svc, role, method, params) },
	})
}

// audit records full's Send, SendAcknowledged and their Revision forms (by draft), ResolveOutbox
// "resend" (by outbox item), Unsubscribe (by message), BeginPluginLogin and the rule calls that
// make a rule forward (ruleForward), and mcp's NewDraft (by source message) and
// SaveDraft (by draft); nothing else. A row it cannot write stops the call.
func audit(svc *core.Service, role, method string, params []json.RawMessage) error {
	if role == rpc.RoleFull {
		target, dests, ok, err := ruleForward(svc, method, params)
		if err != nil {
			return err
		}
		if ok {
			return svc.AuditExternal(context.Background(), core.AuditLocalAPI, method, target, dests...)
		}
	}
	source := ""
	switch {
	case role == rpc.RoleFull && (method == "Send" || method == "SendAcknowledged" || method == "SendRevision" || method == "SendAcknowledgedRevision" ||
		method == "ResolveOutbox" && isResend(params) || method == "Unsubscribe" || method == "BeginPluginLogin"):
		source = core.AuditLocalAPI
	case role == rpc.RoleMCP && (method == "NewDraft" || method == "SaveDraft"):
		source = core.AuditMCP
	default:
		return nil
	}
	var target int64
	switch {
	case method == "NewDraft" && len(params) == 3:
		json.Unmarshal(params[2], &target)
	case method == "SaveDraft" && len(params) == 1:
		var d struct{ ID int64 }
		json.Unmarshal(params[0], &d)
		target = d.ID
	case len(params) > 0:
		json.Unmarshal(params[0], &target)
	}
	return svc.AuditExternal(context.Background(), source, method, target)
}

// ruleForward reports whether a full call leaves a rule that forwards mail
// later, unattended, which ADR 0003 counts as egress; target and dests are
// what its audit row records. They are SaveRule of a rule with a forward
// (target: its id, 0 for a new one, whose id does not exist before the call
// runs; dests: its recipients' domains, which name it in the log), SetRuleEnabled
// turning such a rule on (target: the rule), and every RetryRuleRun (target:
// the run): a retry may forward now, and which actions it reruns is known
// only inside it. RunRuleOnExisting drops forwards and ProposedRuleSaved only
// closes an approval, so neither is here. Params that do not decode are not
// audited: the dispatcher decodes them the same way and refuses the call.
func ruleForward(svc *core.Service, method string, params []json.RawMessage) (target int64, dests []string, ok bool, err error) {
	switch method {
	case "SaveRule":
		var r core.Rule
		if len(params) != 1 || json.Unmarshal(params[0], &r) != nil {
			return 0, nil, false, nil
		}
		dests = forwards(r.Doc)
		return r.ID, dests, len(dests) > 0, nil
	case "SetRuleEnabled":
		var id int64
		var on bool
		if len(params) != 2 || json.Unmarshal(params[0], &id) != nil || json.Unmarshal(params[1], &on) != nil || !on {
			return 0, nil, false, nil
		}
		rules, err := svc.Rules(context.Background())
		if err != nil {
			return 0, nil, false, err
		}
		for _, r := range rules {
			if r.ID == id {
				dests = forwards(r.Doc)
			}
		}
		return id, dests, len(dests) > 0, nil
	case "RetryRuleRun":
		if len(params) != 1 || json.Unmarshal(params[0], &target) != nil {
			return 0, nil, false, nil
		}
		return target, nil, true, nil
	}
	return 0, nil, false, nil
}

// forwards are the domains d forwards to. They are cut here, not by
// gateway.Host: a local part may hold '/', '?' or '#', where Host cuts a URL.
// A recipient that does not parse is left out, its text kept off the log:
// SaveRule refuses a rule with one, so it never forwards.
func forwards(d core.RuleDoc) []string {
	var out []string
	for _, a := range d.Actions {
		if p, err := mail.ParseAddress(a.To); a.Kind == core.ActionForward && err == nil {
			out = append(out, p.Address[strings.LastIndexByte(p.Address, '@')+1:])
		}
	}
	return out
}

// isResend: ResolveOutbox's params ask to send the item again.
func isResend(params []json.RawMessage) bool {
	var action string
	return len(params) == 2 && json.Unmarshal(params[1], &action) == nil && action == "resend"
}
