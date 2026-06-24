package steam

import (
	"crypto/sha1"
	"log"
	"sync/atomic"
	"time"

	"github.com/sicdex/go-steam-ws/protocol"
	"github.com/sicdex/go-steam-ws/protocol/protobuf"
	"github.com/sicdex/go-steam-ws/protocol/steamlang"
	"github.com/sicdex/go-steam-ws/steamid"
	"google.golang.org/protobuf/proto"
)

type Auth struct {
	client  *Client
	details *LogOnDetails
}

type SentryHash []byte

type LogOnDetails struct {
	Username string

	// If logging into an account without a login key, the account's password.
	Password string

	// If you have a Steam Guard email code, you can provide it here.
	AuthCode string

	// If you have a Steam Guard mobile two-factor authentication code, you can provide it here.
	TwoFactorCode  string
	SentryFileHash SentryHash
	LoginKey       string

	// true if you want to get a login key which can be used in lieu of
	// a password for subsequent logins. false or omitted otherwise.
	ShouldRememberPassword bool

	// MachineID is the binary machine_id blob (Valve KeyValues MessageObject)
	// sent with logon. Leave nil to send a stable per-account id derived from
	// Username — recommended for headless pools, so each account consistently
	// presents its own unique "machine". Set it only to override that.
	MachineID []byte

	// AccessToken is a refresh token from the modern credential auth flow
	// (Authentication.BeginAuthSessionViaCredentials → PollAuthSessionStatus).
	// When set, the logon authenticates with the token instead of the password —
	// the only path Steam still accepts for many accounts. Username is still
	// required; Password may be empty.
	AccessToken string
}

// Log on with the given details. You must always specify username and
// password OR username and loginkey. For the first login, don't set an authcode or a hash and you'll
//  receive an error (EResult_AccountLogonDenied)
// and Steam will send you an authcode. Then you have to login again, this time with the authcode.
// Shortly after logging in, you'll receive a MachineAuthUpdateEvent with a hash which allows
// you to login without using an authcode in the future.
//
// If you don't use Steam Guard, username and password are enough.
//
// After the event EMsg_ClientNewLoginKey is received you can use the LoginKey
// to login instead of using the password.
func (a *Auth) LogOn(details *LogOnDetails) {
	if details.Username == "" {
		panic("Username must be set!")
	}
	if details.Password == "" && details.LoginKey == "" && details.AccessToken == "" {
		panic("Password, LoginKey, or AccessToken must be set!")
	}

	logon := new(protobuf.CMsgClientLogon)
	logon.AccountName = &details.Username
	// With a token, Steam wants the password field ABSENT (SteamKit sends none);
	// an empty password present alongside the token gets rejected as InvalidPassword.
	if details.AccessToken == "" {
		logon.Password = &details.Password
	}
	if details.AuthCode != "" {
		logon.AuthCode = proto.String(details.AuthCode)
	}
	if details.TwoFactorCode != "" {
		logon.TwoFactorCode = proto.String(details.TwoFactorCode)
	}
	logon.ClientLanguage = proto.String("english")
	logon.ProtocolVersion = proto.Uint32(steamlang.MsgClientLogon_CurrentProtocol)
	logon.ShaSentryfile = details.SentryFileHash
	if details.LoginKey != "" {
		logon.LoginKey = proto.String(details.LoginKey)
	}
	if details.AccessToken != "" {
		logon.AccessToken = proto.String(details.AccessToken)
	}
	if details.ShouldRememberPassword {
		logon.ShouldRememberPassword = proto.Bool(details.ShouldRememberPassword)
	}

	// Present the client fingerprint a real desktop Steam client sends. Steam's
	// anti-fraud flags accounts that log on with no machine_id — or with an
	// empty/identical one across a whole fleet — and treats a credentialed
	// logon that never establishes a machine as a brand-new untrusted device
	// every time. Giving each account a stable, unique fingerprint makes the
	// pool look like many distinct, returning desktop clients rather than one
	// anonymous automated swarm. Fields mirror SteamKit2's CMsgClientLogon.
	if details.MachineID != nil {
		logon.MachineId = details.MachineID
	} else {
		logon.MachineId = generateMachineID(details.Username)
	}
	logon.MachineName = proto.String(machineName(details.Username))
	// A real Steam client build number — SteamKit notes Steam needs this to
	// hand back a proper sentry file for machine auth.
	logon.ClientPackageVersion = proto.Uint32(1771)
	// Report Windows 10 (EOSType 16) — what the overwhelming majority of
	// Dota/Deadlock clients run, regardless of our Linux host. Steam does not
	// cross-check this against the connection.
	logon.ClientOsType = proto.Uint32(16)
	// chat_mode 2 = the post-2016 "new" friends/chat that the real client
	// always negotiates at logon.
	logon.ChatMode = proto.Uint32(2)
	// obfuscated_private_ip: a real client always reports its (XOR-obfuscated)
	// LAN address here; an absent one is unusual. Synthesize a stable, plausible
	// private address per account. Both the modern CMsgIPAddress field and its
	// deprecated uint32 twin are set, as a real client does.
	obfIP := obfuscatedPrivateIP(details.Username)
	logon.ObfuscatedPrivateIp = &protobuf.CMsgIPAddress{Ip: &protobuf.CMsgIPAddress_V4{V4: obfIP}}
	logon.DeprecatedObfustucatedPrivateIp = proto.Uint32(obfIP)
	// Mirror the real client: OK once we already hold a machine-auth (sentry)
	// hash, FileNotFound on the very first logon.
	if len(details.SentryFileHash) > 0 {
		logon.EresultSentryfile = proto.Int32(int32(steamlang.EResult_OK))
	} else {
		logon.EresultSentryfile = proto.Int32(int32(steamlang.EResult_FileNotFound))
	}

	// Opt in to Steam's dedicated rate-limit responses (matches SteamKit2's
	// default). Without this flag Steam masks login throttling — too many
	// logon attempts from this account or IP — as EResult_InvalidPassword,
	// which is indistinguishable from a genuinely wrong password. With it set,
	// a throttled logon comes back as EResult_RateLimitExceeded /
	// EResult_AccountLoginDeniedThrottle instead, so callers can back off and
	// retry rather than falsely flagging the credentials as bad.
	logon.SupportsRateLimitResponse = proto.Bool(true)

	atomic.StoreUint64(&a.client.steamId, uint64(steamid.NewIdAdv(0, 1, int32(steamlang.EUniverse_Public), int32(steamlang.EAccountType_Individual))))

	a.client.Write(protocol.NewClientMsgProtobuf(steamlang.EMsg_ClientLogon, logon))
}

// LogOnAnonymousGS logs on to Steam as an anonymous game server — no
// username/password. This is the login an app's GC expects for ServerToGC
// messages (the classic "anonymous GS" path). After the LoggedOnEvent the
// caller announces its served app via a CMsgGSServerType and then completes
// the GC ServerHello → ServerWelcome handshake.
func (a *Auth) LogOnAnonymousGS() {
	logon := new(protobuf.CMsgClientLogon)
	logon.ProtocolVersion = proto.Uint32(steamlang.MsgClientLogon_CurrentProtocol)

	// Anonymous game-server identity: zero account id, instance 0, account
	// type AnonGameServer (vs Individual for a credentialed user logon).
	atomic.StoreUint64(&a.client.steamId, uint64(steamid.NewIdAdv(0, 0, int32(steamlang.EUniverse_Public), int32(steamlang.EAccountType_AnonGameServer))))

	a.client.Write(protocol.NewClientMsgProtobuf(steamlang.EMsg_ClientLogon, logon))
}

func (a *Auth) HandlePacket(packet *protocol.Packet) {
	switch packet.EMsg {
	case steamlang.EMsg_ClientLogOnResponse:
		a.handleLogOnResponse(packet)
	case steamlang.EMsg_ClientNewLoginKey:
		a.handleLoginKey(packet)
	case steamlang.EMsg_ClientSessionToken:
	case steamlang.EMsg_ClientLoggedOff:
		a.handleLoggedOff(packet)
	case steamlang.EMsg_ClientUpdateMachineAuth:
		a.handleUpdateMachineAuth(packet)
	case steamlang.EMsg_ClientAccountInfo:
		a.handleAccountInfo(packet)
	}
}

func (a *Auth) handleLogOnResponse(packet *protocol.Packet) {
	if !packet.IsProto {
		a.client.Fatalf("Got non-proto logon response!")
		return
	}

	body := new(protobuf.CMsgClientLogonResponse)
	msg := packet.ReadProtoMsg(body)

	result := steamlang.EResult(body.GetEresult())
	if result != steamlang.EResult_OK && DebugPackets {
		// Surface the exact codes Steam returned — including the "silent"
		// Fail/ServiceUnavailable/TryAnotherCM branch below that emits no event.
		log.Printf("steam: logon response: eresult=%v eresult_extended=%v", result, steamlang.EResult(body.GetEresultExtended()))
	}
	if result == steamlang.EResult_OK {
		atomic.StoreInt32(&a.client.sessionId, msg.Header.Proto.GetClientSessionid())
		atomic.StoreUint64(&a.client.steamId, msg.Header.Proto.GetSteamid())
		// Steam deprecated webapi_authenticate_user_nonce in favor of
		// the new auth ticket flow — modern logon responses leave it
		// nil. Guard so we don't panic dereferencing it, and just keep
		// Web.webLoginKey blank (the Web helpers degrade gracefully).
		if body.WebapiAuthenticateUserNonce != nil {
			a.client.Web.webLoginKey = *body.WebapiAuthenticateUserNonce
		}

		go a.client.heartbeatLoop(time.Duration(body.GetOutOfGameHeartbeatSeconds()))

		a.client.Emit(&LoggedOnEvent{
			Result:                    steamlang.EResult(body.GetEresult()),
			ExtendedResult:            steamlang.EResult(body.GetEresultExtended()),
			OutOfGameSecsPerHeartbeat: body.GetOutOfGameHeartbeatSeconds(),
			InGameSecsPerHeartbeat:    body.GetInGameHeartbeatSeconds(),
			PublicIp:                  body.GetDeprecatedPublicIp(),
			ServerTime:                body.GetRtime32ServerTime(),
			AccountFlags:              steamlang.EAccountFlags(body.GetAccountFlags()),
			ClientSteamId:             steamid.SteamId(body.GetClientSuppliedSteamid()),
			EmailDomain:               body.GetEmailDomain(),
			CellId:                    body.GetCellId(),
			CellIdPingThreshold:       body.GetCellIdPingThreshold(),
			Steam2Ticket:              body.GetSteam2Ticket(),
			UsePics:                   body.GetDeprecatedUsePics(),
			WebApiUserNonce:           body.GetWebapiAuthenticateUserNonce(),
			IpCountryCode:             body.GetIpCountryCode(),
			VanityUrl:                 body.GetVanityUrl(),
			NumLoginFailuresToMigrate: body.GetCountLoginfailuresToMigrate(),
			NumDisconnectsToMigrate:   body.GetCountDisconnectsToMigrate(),
		})
	} else if result == steamlang.EResult_Fail || result == steamlang.EResult_ServiceUnavailable || result == steamlang.EResult_TryAnotherCM {
		// some error on Steam's side, we'll get an EOF later
	} else {
		a.client.Emit(&LogOnFailedEvent{
			Result:         steamlang.EResult(body.GetEresult()),
			ExtendedResult: steamlang.EResult(body.GetEresultExtended()),
		})
		a.client.Disconnect()
	}
}

func (a *Auth) handleLoginKey(packet *protocol.Packet) {
	body := new(protobuf.CMsgClientNewLoginKey)
	packet.ReadProtoMsg(body)
	a.client.Write(protocol.NewClientMsgProtobuf(steamlang.EMsg_ClientNewLoginKeyAccepted, &protobuf.CMsgClientNewLoginKeyAccepted{
		UniqueId: proto.Uint32(body.GetUniqueId()),
	}))
	a.client.Emit(&LoginKeyEvent{
		UniqueId: body.GetUniqueId(),
		LoginKey: body.GetLoginKey(),
	})
}

func (a *Auth) handleLoggedOff(packet *protocol.Packet) {
	result := steamlang.EResult_Invalid
	if packet.IsProto {
		body := new(protobuf.CMsgClientLoggedOff)
		packet.ReadProtoMsg(body)
		result = steamlang.EResult(body.GetEresult())
	} else {
		body := new(steamlang.MsgClientLoggedOff)
		packet.ReadClientMsg(body)
		result = body.Result
	}
	a.client.Emit(&LoggedOffEvent{Result: result})
}

func (a *Auth) handleUpdateMachineAuth(packet *protocol.Packet) {
	body := new(protobuf.CMsgClientUpdateMachineAuth)
	packet.ReadProtoMsg(body)

	// Hash the sentry FILE CONTENT (the bytes Steam sends here) — not the raw
	// packet, as before — and echo a complete response, exactly as a real
	// client does. Steam then trusts this machine, and the returned SHA can be
	// replayed as sha_sentryfile on the next logon to log on as a known device.
	// Steam delivers the sentry in a single chunk in practice; offset/cubtowrite
	// are echoed back for completeness.
	sentry := body.GetBytes()
	sum := sha1.Sum(sentry)
	sha := sum[:]

	msg := protocol.NewClientMsgProtobuf(steamlang.EMsg_ClientUpdateMachineAuthResponse, &protobuf.CMsgClientUpdateMachineAuthResponse{
		Filename:     proto.String(body.GetFilename()),
		Eresult:      proto.Uint32(uint32(steamlang.EResult_OK)),
		Filesize:     proto.Uint32(uint32(len(sentry))),
		ShaFile:      sha,
		Getlasterror: proto.Uint32(0),
		Offset:       proto.Uint32(body.GetOffset()),
		Cubwrote:     proto.Uint32(body.GetCubtowrite()),
	})
	msg.SetTargetJobId(packet.SourceJobId)
	a.client.Write(msg)

	a.client.Emit(&MachineAuthUpdateEvent{sha})
}

func (a *Auth) handleAccountInfo(packet *protocol.Packet) {
	body := new(protobuf.CMsgClientAccountInfo)
	packet.ReadProtoMsg(body)
	a.client.Emit(&AccountInfoEvent{
		PersonaName:          body.GetPersonaName(),
		Country:              body.GetIpCountry(),
		CountAuthedComputers: body.GetCountAuthedComputers(),
		AccountFlags:         steamlang.EAccountFlags(body.GetAccountFlags()),
		FacebookId:           body.GetFacebookId(),
		FacebookName:         body.GetFacebookName(),
	})
}
