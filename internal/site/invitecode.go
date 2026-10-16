package site

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"time"

	"filippo.io/cpace"

	"ibs/internal/relay"
)

// Short invite codes, like "relay.example.com 7-comet-harbor-violet": a relay
// of the inviting site, a number claimed on that relay, and three words.
//
//  1. The inviting site claims the number on its relay for a day and keeps
//     the words and a full invite.
//  2. The joining site connects to the relay naming the number; the relay
//     puts it through to the inviting site (once: the number is then gone).
//  3. Both run CPace, a password-authenticated key exchange, with the number
//     and words as the password: only a site that knows the words gets the
//     key, and a wrong guess burns the code. The relay, which only passes the
//     bytes along, learns nothing it could use.
//  4. Each proves it has the key; the inviting site then sends the full
//     invite, encrypted with it, and joining continues as with a long invite.
//
// Sites without a relay that supports codes (relay 1.4.0 or newer) hand out
// the long invite instead; joining accepts both.

const codeWordCount = 3

var cpaceContext = cpace.NewContextInfo("ibs-joining-site", "ibs-inviting-site", []byte("ibs invite code v1"))

// CreateInviteCode makes a short invite code on one of this site's relays.
func (s *Site) CreateInviteCode(ctx context.Context) (string, error) {
	var rc *relay.Client
	for _, c := range s.relayClients() {
		if c.Connected() && (rc == nil || c.RTT() < rc.RTT()) {
			rc = c
		}
	}
	if rc == nil {
		return "", errors.New("short invite codes need a connected relay")
	}
	invite, err := s.CreateInvite()
	if err != nil {
		return "", err
	}
	words := make([]string, codeWordCount)
	for i := range words {
		words[i] = codeWords[randomInt(len(codeWords))]
	}
	expires := time.Now().Add(inviteTTL)
	for range 8 {
		number := fmt.Sprint(1 + randomInt(999))
		var taken int
		s.DB.QueryRow(`SELECT COUNT(*) FROM invite_codes WHERE number = ? AND expires > ?`, number, time.Now().Unix()).Scan(&taken)
		if taken > 0 {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := rc.ClaimCode(cctx, number, expires)
		cancel()
		if err != nil && err.Error() == "taken" {
			continue // another site on this relay uses that number
		}
		if err != nil {
			return "", err
		}
		if _, err := s.DB.Exec(`INSERT INTO invite_codes(number, relay_id, words, invite, expires) VALUES(?, ?, ?, ?, ?)
			ON CONFLICT(number) DO UPDATE SET relay_id = excluded.relay_id, words = excluded.words, invite = excluded.invite,
				expires = excluded.expires, used = 0`,
			number, rc.ID(), strings.Join(words, "-"), invite, expires.Unix()); err != nil {
			return "", err
		}
		s.DB.Exec(`DELETE FROM invite_codes WHERE expires < ?`, time.Now().Unix())
		return formatCode(rc.Token.Address, number, words), nil
	}
	return "", errors.New("no free invite code number on the relay; try again")
}

// reclaimCodes claims this site's open invite codes again on a relay that
// (re)connected, e.g. after the relay restarted and forgot them.
func (s *Site) reclaimCodes(rc *relay.Client) {
	rows, err := s.DB.Query(`SELECT number, expires FROM invite_codes WHERE relay_id = ? AND used = 0 AND expires > ?`, rc.ID(), time.Now().Unix())
	if err != nil {
		return
	}
	type code struct {
		number  string
		expires int64
	}
	var codes []code
	for rows.Next() {
		var c code
		rows.Scan(&c.number, &c.expires)
		codes = append(codes, c)
	}
	rows.Close()
	for _, c := range codes {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := rc.ClaimCode(ctx, c.number, time.Unix(c.expires, 0)); err != nil {
			s.log.Warn("claim invite code again", "relay", rc.Token.Address, "err", err)
		}
		cancel()
	}
}

// answerCode is the inviting site's side: a joining site came through the
// relay for invite code number.
func (s *Site) answerCode(conn net.Conn, number string) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(peerTimeout))
	var words, invite string
	err := s.DB.QueryRow(`SELECT words, invite FROM invite_codes WHERE number = ? AND used = 0 AND expires > ?`, number, time.Now().Unix()).
		Scan(&words, &invite)
	if err != nil {
		return
	}
	// One attempt: the code is spent whether or not the words match.
	s.DB.Exec(`UPDATE invite_codes SET used = 1 WHERE number = ?`, number)
	msgA, err := readFrame(conn)
	if err != nil {
		return
	}
	msgB, key, err := cpace.Exchange(number+"-"+words, cpaceContext, msgA)
	if err != nil || writeFrame(conn, msgB) != nil {
		return
	}
	proof, err := readFrame(conn)
	if err != nil {
		return
	}
	if !hmac.Equal(proof, codeProof(key, "joining")) {
		s.log.Warn("invite code used with the wrong words; it can't be used again", "code", number)
		return
	}
	sealed, err := seal(key, []byte(invite))
	if err != nil || writeFrame(conn, codeProof(key, "inviting")) != nil || writeFrame(conn, sealed) != nil {
		return
	}
	s.log.Info("invite code used", "code", number)
}

// JoinWithCode joins the IBS with a short invite code.
func (s *Site) JoinWithCode(ctx context.Context, code string) (*PeerInfo, error) {
	address, number, words, err := parseCode(code)
	if err != nil {
		return nil, err
	}
	cert, err := s.ID.Certificate()
	if err != nil {
		return nil, err
	}
	conn, err := relay.DialCode(ctx, address, number, cert)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	invite, err := fetchInvite(conn, number, words)
	if err != nil {
		return nil, err
	}
	return s.JoinWithInvite(ctx, invite)
}

// fetchInvite is the joining site's side of the exchange: it proves it knows
// the words and gets the full invite.
func fetchInvite(conn net.Conn, number, words string) (string, error) {
	conn.SetDeadline(time.Now().Add(peerTimeout))
	msgA, st, err := cpace.Start(number+"-"+words, cpaceContext)
	if err != nil {
		return "", err
	}
	if err := writeFrame(conn, msgA); err != nil {
		return "", err
	}
	msgB, err := readFrame(conn)
	if err != nil {
		return "", fmt.Errorf("the inviting site didn't answer: %w", err)
	}
	key, err := st.Finish(msgB)
	if err != nil {
		return "", err
	}
	if err := writeFrame(conn, codeProof(key, "joining")); err != nil {
		return "", err
	}
	proof, err := readFrame(conn)
	if err != nil || !hmac.Equal(proof, codeProof(key, "inviting")) {
		return "", errors.New("the code's words don't match, so it can't be used any more: ask for a new code")
	}
	sealed, err := readFrame(conn)
	if err != nil {
		return "", err
	}
	invite, err := open(key, sealed)
	if err != nil {
		return "", errors.New("the invite didn't arrive intact; ask for a new code")
	}
	return string(invite), nil
}

// Join joins the IBS with either a short invite code or a long invite.
func (s *Site) Join(ctx context.Context, text string) (*PeerInfo, error) {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, invitePrefix) {
		return s.JoinWithInvite(ctx, t)
	}
	return s.JoinWithCode(ctx, t)
}

// formatCode writes a code; the relay's default port is left out.
func formatCode(address, number string, words []string) string {
	address = strings.TrimSuffix(address, ":7444")
	return address + " " + number + "-" + strings.Join(words, "-")
}

// parseCode reads "relay.example.com[:port] 7-comet-harbor-violet".
func parseCode(code string) (address, number, words string, err error) {
	bad := errors.New(`not an invite code: expected something like "relay.example.com 7-comet-harbor-violet", or a long invite (ibs2.…)`)
	f := strings.Fields(strings.ToLower(code))
	if len(f) != 2 {
		return "", "", "", bad
	}
	address = relayAddr(f[0])
	parts := strings.Split(f[1], "-")
	if len(parts) != 1+codeWordCount || !validCodeNumber(parts[0]) {
		return "", "", "", bad
	}
	for _, w := range parts[1:] {
		if !isCodeWord(w) {
			return "", "", "", fmt.Errorf("%q isn't one of the code words: check the spelling", w)
		}
	}
	return address, parts[0], strings.Join(parts[1:], "-"), nil
}

func isCodeWord(w string) bool {
	for _, c := range codeWords {
		if c == w {
			return true
		}
	}
	return false
}

func validCodeNumber(s string) bool {
	if len(s) == 0 || len(s) > 4 || s[0] == '0' {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func randomInt(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic(err)
	}
	return int(v.Int64())
}

// codeProof shows the other site this one derived the same key.
func codeProof(key []byte, side string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("ibs invite code proof: " + side))
	return m.Sum(nil)
}

func codeAEAD(key []byte) (cipher.AEAD, error) {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("ibs invite code encryption"))
	b, err := aes.NewCipher(m.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func seal(key, plain []byte) ([]byte, error) {
	g, err := codeAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	rand.Read(nonce)
	return g.Seal(nonce, nonce, plain, nil), nil
}

func open(key, sealed []byte) ([]byte, error) {
	g, err := codeAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < g.NonceSize() {
		return nil, errors.New("too short")
	}
	return g.Open(nil, sealed[:g.NonceSize()], sealed[g.NonceSize():], nil)
}

// Frames: a 2-byte length, then the bytes.
func writeFrame(w io.Writer, b []byte) error {
	if len(b) > 0xffff {
		return errors.New("frame too large")
	}
	var n [2]byte
	binary.BigEndian.PutUint16(n[:], uint16(len(b)))
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(n[:]))
	_, err := io.ReadFull(r, b)
	return b, err
}
