package steam

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/sicdex/go-steam-ws/protocol"
	"github.com/sicdex/go-steam-ws/protocol/protobuf/unified"
	"github.com/sicdex/go-steam-ws/protocol/steamlang"
	"google.golang.org/protobuf/proto"
)

// emsgServiceMethodCallFromClientNonAuthed is the EMsg for a unified
// service-method call made BEFORE login (no session yet). Steam routes the call
// by the proto header's target_job_name (e.g. "Authentication.BeginAuthSessionViaCredentials#1")
// and replies with EMsg_ServiceMethodResponse carrying jobid_target = our jobid_source.
// Not in the fork's generated steamlang enums, so defined here.
const emsgServiceMethodCallFromClientNonAuthed = steamlang.EMsg(9804)

// emsgClientHello (9805) is the ClientHello the real client sends first on a
// fresh connection. Steam's WebSocket CM closes the connection if an
// unauthenticated service call arrives before a ClientHello, so we send one
// (empty header, just the protocol version) before the auth handshake.
const emsgClientHello = steamlang.EMsg(9805)

// Authentication implements Steam's modern credential auth flow
// (IAuthenticationService): RSA-encrypt the password, BeginAuthSessionViaCredentials,
// poll until a refresh token is issued. The refresh token is then used as the
// access_token in a normal CMsgClientLogon — the legacy username+password logon
// is no longer accepted for many accounts.
type Authentication struct {
	client *Client

	mu      sync.Mutex
	pending map[protocol.JobId]chan *protocol.Packet
}

func newAuthentication(c *Client) *Authentication {
	return &Authentication{client: c, pending: make(map[protocol.JobId]chan *protocol.Packet)}
}

// HandlePacket routes EMsg_ServiceMethodResponse replies back to the waiting
// call() by their jobid_target.
func (a *Authentication) HandlePacket(packet *protocol.Packet) {
	if packet.EMsg != steamlang.EMsg_ServiceMethodResponse {
		return
	}
	a.mu.Lock()
	ch, ok := a.pending[packet.TargetJobId]
	a.mu.Unlock()
	if ok {
		select {
		case ch <- packet:
		default:
		}
	}
}

// call sends one unauthenticated unified service-method request and waits for
// its response, returning the header EResult and unmarshaling the body into resp.
func (a *Authentication) call(method string, req, resp proto.Message) (steamlang.EResult, error) {
	jobID := a.client.GetNextJobId()
	ch := make(chan *protocol.Packet, 1)
	a.mu.Lock()
	a.pending[jobID] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, jobID)
		a.mu.Unlock()
	}()

	msg := protocol.NewClientMsgProtobuf(emsgServiceMethodCallFromClientNonAuthed, req)
	msg.Header.Proto.TargetJobName = proto.String(method)
	msg.SetSourceJobId(jobID)
	a.sendNoStamp(msg)

	select {
	case packet := <-ch:
		cm := packet.ReadProtoMsg(resp)
		return steamlang.EResult(cm.Header.Proto.GetEresult()), nil
	case <-time.After(30 * time.Second):
		return 0, fmt.Errorf("steam: service method %q timed out", method)
	}
}

// sendNoStamp queues a message WITHOUT stamping the client's steamid/sessionid
// onto its header (which Client.Write always does). Steam drops a pre-login
// unified service call that carries a zero steamid/client_sessionid; SteamKit
// leaves both absent until logged in, and so must we.
func (a *Authentication) sendNoStamp(msg protocol.IMsg) {
	a.client.mutex.RLock()
	defer a.client.mutex.RUnlock()
	if a.client.conn == nil {
		return
	}
	a.client.writeChan <- msg
}

// CredentialsAuthResult is what a successful credential auth yields. The
// RefreshToken goes into CMsgClientLogon.access_token for the actual logon.
type CredentialsAuthResult struct {
	RefreshToken string
	AccessToken  string
	AccountName  string
	SteamID      uint64
}

// LogOnWithCredentials runs the full modern credential-auth handshake and
// returns the issued tokens. The client must already be connected (encrypted
// channel up). Call this from a goroutine separate from the Events() reader so
// the read loop can deliver the service-method responses.
//
// It deliberately does NOT proceed when the account requires an interactive
// Steam Guard confirmation (email/device code), since a headless pool can't
// satisfy it — the caller gets a descriptive error instead of hanging.
func (a *Authentication) LogOnWithCredentials(username, password string) (*CredentialsAuthResult, error) {
	// 0. Greet the CM. Without a ClientHello first, Steam's WebSocket CM closes
	// the connection when it receives an unauthenticated service call. Empty
	// header (no steamid/sessionid), just the protocol version — as the real
	// client sends.
	a.sendNoStamp(protocol.NewClientMsgProtobuf(emsgClientHello, &unified.CMsgClientHello{
		ProtocolVersion: proto.Uint32(steamlang.MsgClientLogon_CurrentProtocol),
	}))

	// 1. Fetch the account's RSA public key.
	rsaResp := &unified.CAuthentication_GetPasswordRSAPublicKey_Response{}
	if res, err := a.call(
		"Authentication.GetPasswordRSAPublicKey#1",
		&unified.CAuthentication_GetPasswordRSAPublicKey_Request{AccountName: proto.String(username)},
		rsaResp,
	); err != nil {
		return nil, err
	} else if res != steamlang.EResult_OK {
		return nil, fmt.Errorf("GetPasswordRSAPublicKey: %v", res)
	}

	// 2. RSA-encrypt the password with that key.
	encPassword, err := encryptPassword(password, rsaResp.GetPublickeyMod(), rsaResp.GetPublickeyExp())
	if err != nil {
		return nil, err
	}

	// 3. Begin the auth session.
	begin := &unified.CAuthentication_BeginAuthSessionViaCredentials_Response{}
	if res, err := a.call(
		"Authentication.BeginAuthSessionViaCredentials#1",
		&unified.CAuthentication_BeginAuthSessionViaCredentials_Request{
			AccountName:         proto.String(username),
			EncryptedPassword:   proto.String(encPassword),
			EncryptionTimestamp: rsaResp.Timestamp,
			Persistence:         unified.ESessionPersistence_k_ESessionPersistence_Persistent.Enum(),
			WebsiteId:           proto.String("Client"),
			DeviceFriendlyName:  proto.String(machineName(username)),
			PlatformType:        unified.EAuthTokenPlatformType_k_EAuthTokenPlatformType_SteamClient.Enum(),
			DeviceDetails: &unified.CAuthentication_DeviceDetails{
				DeviceFriendlyName: proto.String(machineName(username)),
				PlatformType:       unified.EAuthTokenPlatformType_k_EAuthTokenPlatformType_SteamClient.Enum(),
				OsType:             proto.Int32(16), // Windows 10
			},
		},
		begin,
	); err != nil {
		return nil, err
	} else if res != steamlang.EResult_OK {
		// Wrong password / banned surfaces here as InvalidPassword, same as the
		// legacy path — but now distinguishable from a dead legacy path.
		return nil, fmt.Errorf("BeginAuthSessionViaCredentials: %v", res)
	}

	// A guard-free account allows only None/Unknown confirmations. Anything else
	// (email/device code) needs interactive input we can't provide headlessly.
	for _, c := range begin.GetAllowedConfirmations() {
		switch c.GetConfirmationType() {
		case unified.EAuthSessionGuardType_k_EAuthSessionGuardType_None,
			unified.EAuthSessionGuardType_k_EAuthSessionGuardType_Unknown:
		default:
			return nil, fmt.Errorf("account %q requires Steam Guard (%v); token-auth needs a guard-free account", username, c.GetConfirmationType())
		}
	}

	// 4. Poll until the refresh token is issued.
	interval := time.Duration(float64(begin.GetInterval()) * float64(time.Second))
	if interval < time.Second {
		interval = time.Second
	}
	for attempt := 0; attempt < 30; attempt++ {
		poll := &unified.CAuthentication_PollAuthSessionStatus_Response{}
		if res, err := a.call(
			"Authentication.PollAuthSessionStatus#1",
			&unified.CAuthentication_PollAuthSessionStatus_Request{
				ClientId:  begin.ClientId,
				RequestId: begin.RequestId,
			},
			poll,
		); err != nil {
			return nil, err
		} else if res != steamlang.EResult_OK {
			return nil, fmt.Errorf("PollAuthSessionStatus: %v", res)
		}
		if poll.GetRefreshToken() != "" {
			name := poll.GetAccountName()
			if name == "" {
				name = username
			}
			return &CredentialsAuthResult{
				RefreshToken: poll.GetRefreshToken(),
				AccessToken:  poll.GetAccessToken(),
				AccountName:  name,
				SteamID:      begin.GetSteamid(),
			}, nil
		}
		time.Sleep(interval)
	}
	return nil, fmt.Errorf("token-auth: timed out waiting for the auth session to be approved")
}

// encryptPassword RSA-encrypts the password with the hex-encoded modulus and
// exponent Steam returns, using PKCS#1 v1.5 padding, and base64-encodes the
// result — exactly as SteamKit / the official client do.
func encryptPassword(password, modHex, expHex string) (string, error) {
	modBytes, err := hex.DecodeString(modHex)
	if err != nil {
		return "", fmt.Errorf("decode rsa modulus: %w", err)
	}
	expBytes, err := hex.DecodeString(expHex)
	if err != nil {
		return "", fmt.Errorf("decode rsa exponent: %w", err)
	}
	pub := &rsa.PublicKey{
		N: new(big.Int).SetBytes(modBytes),
		E: int(new(big.Int).SetBytes(expBytes).Int64()),
	}
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(password))
	if err != nil {
		return "", fmt.Errorf("rsa encrypt password: %w", err)
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}
