package notifytest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type SMTPMode int

const (
	SMTPPlain SMTPMode = iota
	SMTPStartTLS
	SMTPImplicitTLS
)

type Mail struct {
	From string
	To   []string
	Data string
}

type Login struct {
	Username string
	Password string
}

type SMTPServer struct {
	Host      string
	Port      int
	ClientTLS *tls.Config
	ctx       context.Context
	mode      SMTPMode
	serverTLS *tls.Config
	listener  net.Listener
	wg        sync.WaitGroup
	mu        sync.Mutex
	open      map[net.Conn]bool
	mails     []Mail
	logins    []Login
}

func NewSMTPServer(t *testing.T, mode SMTPMode) *SMTPServer {
	t.Helper()
	serverTLS, clientTLS := certificates(t)
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if mode == SMTPImplicitTLS {
		listener = tls.NewListener(listener, serverTLS)
	}
	address := listener.Addr().(*net.TCPAddr)
	s := &SMTPServer{Host: "127.0.0.1", Port: address.Port, ClientTLS: clientTLS, ctx: t.Context(), mode: mode, serverTLS: serverTLS, listener: listener, open: map[net.Conn]bool{}}
	s.wg.Go(s.accept)
	t.Cleanup(s.close)
	return s
}

func (s *SMTPServer) close() {
	_ = s.listener.Close()
	s.mu.Lock()
	for conn := range s.open {
		_ = conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *SMTPServer) Mails() []Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.mails)
}

func (s *SMTPServer) Logins() []Login {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.logins)
}

func (m Mail) Parse() (mail.Header, string, error) {
	message, err := mail.ReadMessage(strings.NewReader(m.Data))
	if err != nil {
		return nil, "", err
	}
	body := message.Body
	encodings := map[string]func(io.Reader) io.Reader{
		"quoted-printable": func(r io.Reader) io.Reader { return quotedprintable.NewReader(r) },
		"base64":           func(r io.Reader) io.Reader { return base64.NewDecoder(base64.StdEncoding, r) },
	}
	if decode, ok := encodings[strings.ToLower(message.Header.Get("Content-Transfer-Encoding"))]; ok {
		body = decode(body)
	}
	text, err := io.ReadAll(body)
	if err != nil {
		return nil, "", err
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
	if err != nil {
		return nil, "", err
	}
	message.Header["Subject"] = []string{subject}
	return message.Header, strings.ReplaceAll(string(text), "\r\n", "\n"), nil
}

func (s *SMTPServer) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Go(func() { s.serve(conn) })
	}
}

type session struct {
	server    *SMTPServer
	conn      net.Conn
	text      *textproto.Conn
	encrypted bool
	mail      Mail
}

func (s *SMTPServer) serve(conn net.Conn) {
	s.mu.Lock()
	s.open[conn] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.open, conn)
		s.mu.Unlock()
	}()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	sess := &session{server: s, conn: conn, text: textproto.NewConn(conn), encrypted: s.mode == SMTPImplicitTLS}
	defer func() { _ = sess.conn.Close() }()
	sess.reply("220 monitor-hub test SMTP")
	for {
		line, err := sess.text.ReadLine()
		if err != nil {
			return
		}
		verb, argument, _ := strings.Cut(line, " ")
		if !sess.handle(strings.ToUpper(verb), argument) {
			return
		}
	}
}

func (sess *session) handle(verb, argument string) bool {
	commands := map[string]func(string){
		"EHLO":     sess.hello,
		"HELO":     sess.hello,
		"STARTTLS": sess.startTLS,
		"AUTH":     sess.auth,
		"MAIL":     func(arg string) { sess.mail = Mail{From: address(arg)}; sess.reply("250 ok") },
		"RCPT":     func(arg string) { sess.mail.To = append(sess.mail.To, address(arg)); sess.reply("250 ok") },
		"DATA":     sess.data,
		"RSET":     func(string) { sess.mail = Mail{}; sess.reply("250 ok") },
		"NOOP":     func(string) { sess.reply("250 ok") },
	}
	if verb == "QUIT" {
		sess.reply("221 bye")
		return false
	}
	command, ok := commands[verb]
	if !ok {
		sess.reply("502 unknown command")
		return true
	}
	command(argument)
	return true
}

func (sess *session) hello(string) {
	lines := []string{"localhost"}
	if sess.server.mode == SMTPStartTLS && !sess.encrypted {
		lines = append(lines, "STARTTLS")
	}
	if sess.encrypted {
		lines = append(lines, "AUTH PLAIN")
	}
	lines = append(lines, "8BITMIME")
	for i, line := range lines {
		separator := "-"
		if i == len(lines)-1 {
			separator = " "
		}
		sess.reply("250" + separator + line)
	}
}

func (sess *session) startTLS(string) {
	if sess.server.mode != SMTPStartTLS || sess.encrypted {
		sess.reply("502 not available")
		return
	}
	sess.reply("220 ready")
	secure := tls.Server(sess.conn, sess.server.serverTLS)
	if err := secure.HandshakeContext(sess.server.ctx); err != nil {
		return
	}
	sess.conn, sess.text, sess.encrypted, sess.mail = secure, textproto.NewConn(secure), true, Mail{}
}

func (sess *session) auth(argument string) {
	mechanism, initial, _ := strings.Cut(argument, " ")
	if !sess.encrypted || !strings.EqualFold(mechanism, "PLAIN") {
		sess.reply("504 unsupported")
		return
	}
	if initial == "" {
		sess.reply("334 ")
		line, err := sess.text.ReadLine()
		if err != nil {
			return
		}
		initial = line
	}
	decoded, err := base64.StdEncoding.DecodeString(initial)
	parts := strings.Split(string(decoded), "\x00")
	if err != nil || len(parts) != 3 {
		sess.reply("501 malformed")
		return
	}
	sess.server.mu.Lock()
	sess.server.logins = append(sess.server.logins, Login{Username: parts[1], Password: parts[2]})
	sess.server.mu.Unlock()
	sess.reply("235 accepted")
}

func (sess *session) data(string) {
	sess.reply("354 end with a dot")
	raw, err := sess.text.ReadDotBytes()
	if err != nil {
		return
	}
	sess.mail.Data = string(raw)
	sess.server.mu.Lock()
	sess.server.mails = append(sess.server.mails, sess.mail)
	sess.server.mu.Unlock()
	sess.mail = Mail{}
	sess.reply("250 queued")
}

func (sess *session) reply(line string) {
	_ = sess.text.PrintfLine("%s", line)
}

func address(argument string) string {
	_, rest, _ := strings.Cut(argument, "<")
	value, _, _ := strings.Cut(rest, ">")
	return value
}

func certificates(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "monitor-hub test SMTP"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	server := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	client := &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	return server, client
}
