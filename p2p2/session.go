package p2p2

import (
	"encoding/hex"
	"time"
)

// A runtime network session auth between 2 peers
// Sessions may be used between 'connections' until they expire
// Session provides the encryptor/decryptor for all messages sent between peers
type NetworkSession interface {
	String() string     // unique session id
	Iv() []byte         // session iv (initiator generated)
	KeyE() []byte       // session shared sym key for enc - 32 bytes
	KeyM() []byte       // session shared sym key for mac - 32 bytes
	PubKey() []byte     // 65 bytes session-only pub key uncompressed
	Created() time.Time // time when session was established

	// TODO: add expiration support

	IsAuthenticated() bool
	SetAuthenticated(val bool)
}

type NetworkSessionImpl struct {
	iv            []byte
	keyE          []byte
	keyM          []byte
	pubKey        []byte
	created       time.Time
	authenticated bool

	// todo: this type might include a decryptor and an encryptor for fast enc/dec of data to/from a remote node
	// when we have an active session - it might be expensive to create these for each outgoing / incoming message
	// there should only be 1 session per remote node
}

func (n *NetworkSessionImpl) String() string {
	return hex.EncodeToString(n.iv)
}

func (n *NetworkSessionImpl) Iv() []byte {
	return n.iv
}

func (n *NetworkSessionImpl) KeyE() []byte {
	return n.keyE
}

func (n *NetworkSessionImpl) KeyM() []byte {
	return n.keyM
}

func (n *NetworkSessionImpl) PubKey() []byte {
	return n.pubKey
}

func (n *NetworkSessionImpl) IsAuthenticated() bool {
	return n.authenticated
}

func (n *NetworkSessionImpl) SetAuthenticated(val bool) {
	n.authenticated = val
}

func (n *NetworkSessionImpl) Created() time.Time {
	return n.created
}

func NewNetworkSession(iv []byte, keyE []byte, keyM []byte, pubKey []byte) NetworkSession {
	s := &NetworkSessionImpl{
		iv:            iv,
		keyE:          keyE,
		keyM:          keyM,
		pubKey:        pubKey,
		created:       time.Now(),
		authenticated: false,
	}

	// todo: create dec/enc here
	return s
}