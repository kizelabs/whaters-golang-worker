package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"wago-worker/whatsapp-service/internal/config"
	"wago-worker/whatsapp-service/internal/httpapi"
	"wago-worker/whatsapp-service/internal/lease"
	sessionpkg "wago-worker/whatsapp-service/internal/session"
	"wago-worker/whatsapp-service/internal/webhook"

	"github.com/lib/pq"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	waStore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var errNotOwner = errors.New("session is owned by another instance")
var schemaIdentPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type Server struct {
	cfg      config.Config
	log      *slog.Logger
	mu       sync.Mutex
	sessions map[string]*waSession
	lease    lease.Coordinator
	store    *sqlstore.Container
}

type waSession struct {
	ctx       context.Context
	cancel    context.CancelFunc
	id        string
	client    *whatsmeow.Client
	qr        string
	connected bool
	lease     *sessionLease
}

type sessionLease struct {
	token  int64
	cancel context.CancelFunc
}

type sendRequest struct {
	SessionID string       `json:"sessionId"`
	To        string       `json:"to"`
	Text      string       `json:"text,omitempty"`
	Media     *mediaObject `json:"media,omitempty"`
}

type mediaObject struct {
	Key       string `json:"key,omitempty"`
	URL       string `json:"url,omitempty"`
	MimeType  string `json:"mimeType"`
	FileName  string `json:"fileName,omitempty"`
	Base64    string `json:"base64,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

type inboundMessage struct {
	SessionID string       `json:"sessionId"`
	MessageID string       `json:"messageId"`
	From      string       `json:"from"`
	Chat      string       `json:"chat"`
	Timestamp string       `json:"timestamp"`
	Text      string       `json:"text,omitempty"`
	Media     *mediaObject `json:"media,omitempty"`
}

func New(cfg config.Config, logger *slog.Logger) (*Server, error) {
	srv := &Server{
		cfg:      cfg,
		log:      logger,
		sessions: make(map[string]*waSession),
	}

	if cfg.IsPostgresStore() {
		leaseCoordinator, err := lease.NewPostgresCoordinator(cfg.DatabaseURL, cfg.DatabaseSchema, cfg.InstanceID, cfg.LeaseTTL, cfg.LeaseStaleGrace)
		if err != nil {
			return nil, err
		}
		srv.lease = leaseCoordinator

		store, err := initPostgresStore(context.Background(), cfg.DatabaseURL, cfg.DatabaseSchema)
		if err != nil {
			return nil, err
		}
		srv.store = store
	}
	return srv, nil
}

func initPostgresStore(ctx context.Context, databaseURL, databaseSchema string) (*sqlstore.Container, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	const lockKey int64 = 740031231991
	lockCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	if _, err := db.ExecContext(lockCtx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to acquire startup advisory lock: %w", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)
	}()

	if databaseSchema != "" {
		if !schemaIdentPattern.MatchString(databaseSchema) {
			_ = db.Close()
			return nil, fmt.Errorf("invalid DATABASE_SCHEMA identifier")
		}
		// Ensure whatsmeow sqlstore migrations/tables resolve to the configured schema.
		if _, err := db.ExecContext(lockCtx, fmt.Sprintf("SET search_path TO %s, public", pq.QuoteIdentifier(databaseSchema))); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to set search_path: %w", err)
		}
	}

	store := sqlstore.NewWithDB(db, "postgres", waLog.Stdout("Database", "INFO", true))
	if err := store.Upgrade(lockCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to upgrade database: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	return store, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)
	mux.HandleFunc("POST /sessions/{sessionId}", s.withAuth(s.handleCreateSession))
	mux.HandleFunc("GET /sessions/{sessionId}/qr", s.withAuth(s.handleQR))
	mux.HandleFunc("POST /sessions/{sessionId}/logout", s.withAuth(s.handleLogout))
	mux.HandleFunc("POST /messages/send", s.withAuth(s.handleSend))
	return httpapi.WithRequestID(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := make(map[string]bool, len(s.sessions))
	for id, session := range s.sessions {
		sessions[id] = session.connected
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "sessions": sessions})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.lease != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.lease.DB().PingContext(ctx); err != nil {
			httpapi.WriteError(w, http.StatusServiceUnavailable, "dependency_unavailable", "lease backend unavailable", true)
			return
		}
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	sessionID := normalizeSessionID(r.PathValue("sessionId"))
	if sessionID == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "sessionId is required", false)
		return
	}
	session, err := s.getOrCreateSession(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, errNotOwner) {
			httpapi.WriteError(w, http.StatusConflict, "not_owner", err.Error(), true)
			return
		}
		httpapi.WriteError(w, http.StatusInternalServerError, "internal_error", err.Error(), true)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "sessionId": session.id, "phoneNumber": session.phoneNumber(), "connected": session.connected, "qr": session.qr})
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	sessionID := normalizeSessionID(r.PathValue("sessionId"))
	session, err := s.getOrCreateSession(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, errNotOwner) {
			httpapi.WriteError(w, http.StatusConflict, "not_owner", err.Error(), true)
			return
		}
		httpapi.WriteError(w, http.StatusInternalServerError, "internal_error", err.Error(), true)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"sessionId": session.id, "phoneNumber": session.phoneNumber(), "connected": session.connected, "qr": session.qr})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sessionID := normalizeSessionID(r.PathValue("sessionId"))
	session, err := s.getOrCreateSession(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, errNotOwner) {
			httpapi.WriteError(w, http.StatusConflict, "not_owner", err.Error(), true)
			return
		}
		httpapi.WriteError(w, http.StatusNotFound, "not_found", err.Error(), false)
		return
	}
	session.cancel()
	if session.lease != nil && session.lease.cancel != nil {
		session.lease.cancel()
		_ = s.lease.Release(r.Context(), sessionID, session.lease.token)
	}
	if err := session.client.Logout(r.Context()); err != nil {
		httpapi.WriteError(w, http.StatusBadGateway, "upstream_failed", err.Error(), true)
		return
	}
	_ = s.deleteSessionDevice(r.Context(), sessionID)
	session.client.Disconnect()
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "sessionId": sessionID})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid JSON", false)
		return
	}
	req.SessionID = normalizeSessionID(req.SessionID)
	if req.SessionID == "" || req.To == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "sessionId and to are required", false)
		return
	}
	session, err := s.session(req.SessionID)
	if err != nil {
		httpapi.WriteError(w, http.StatusNotFound, "not_found", err.Error(), false)
		return
	}
	jid, err := phoneToJID(req.To)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), false)
		return
	}
	msg := &waE2E.Message{}
	if req.Media != nil {
		data, err := s.fetchMedia(r.Context(), req.Media)
		if err != nil {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), false)
			return
		}
		msg, err = session.mediaMessage(r.Context(), data, req.Media, req.Text)
		if err != nil {
			httpapi.WriteError(w, http.StatusBadGateway, "upstream_failed", err.Error(), true)
			return
		}
	} else {
		msg.Conversation = proto.String(req.Text)
	}
	resp, err := session.client.SendMessage(r.Context(), jid, msg)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadGateway, "upstream_failed", err.Error(), true)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "messageId": resp.ID, "timestamp": resp.Timestamp})
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.ServiceToken != "" && r.Header.Get("X-Service-Token") != s.cfg.ServiceToken {
			httpapi.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized", false)
			return
		}
		next(w, r)
	}
}

func (s *Server) getOrCreateSession(ctx context.Context, sessionID string) (*waSession, error) {
	s.mu.Lock()
	existing := s.sessions[sessionID]
	s.mu.Unlock()
	if existing != nil {
		return existing, nil
	}
	var ownedLease *sessionLease
	if s.lease != nil {
		token, ok, err := s.lease.Acquire(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: %s", errNotOwner, sessionID)
		}
		ownedLease = &sessionLease{token: token}
	}
	container, err := s.storeForSession(ctx, sessionID)
	if err != nil {
		if ownedLease != nil {
			_ = s.lease.Release(context.Background(), sessionID, ownedLease.token)
		}
		return nil, err
	}
	device, err := s.deviceForSession(ctx, container, sessionID)
	if err != nil {
		return nil, err
	}
	client := whatsmeow.NewClient(device, waLog.Stdout("Client "+sessionID, "INFO", true))
	sessionCtx, cancel := context.WithCancel(context.Background())
	session := &waSession{id: sessionID, ctx: sessionCtx, cancel: cancel, client: client, lease: ownedLease}
	client.AddEventHandler(s.eventHandler(session))
	s.mu.Lock()
	s.sessions[sessionID] = session
	s.mu.Unlock()
	if session.lease != nil {
		renewCtx, renewCancel := context.WithCancel(context.Background())
		session.lease.cancel = renewCancel
		go s.runLeaseHeartbeat(renewCtx, session)
	}
	if client.Store.ID == nil {
		qrChan, err := client.GetQRChannel(sessionCtx)
		if err != nil {
			return nil, err
		}
		if err := client.Connect(); err != nil {
			cancel()
			return nil, err
		}
		go func() {
			defer cancel()
			for evt := range qrChan {
				if evt.Event == "code" {
					s.mu.Lock()
					session.qr = evt.Code
					s.mu.Unlock()
				}
			}
		}()
	} else if err := client.Connect(); err != nil {
		cancel()
		return nil, err
	}
	return session, nil
}

func (s *Server) storeForSession(ctx context.Context, sessionID string) (*sqlstore.Container, error) {
	if s.cfg.IsPostgresStore() {
		if s.store == nil {
			return nil, errors.New("postgres store is not initialized")
		}
		return s.store, nil
	}
	dbPath := filepath.Join(s.cfg.DataDir, sessionID+".db")
	return sqlstore.New(ctx, "sqlite3", "file:"+dbPath+"?_foreign_keys=on", waLog.Stdout("Database "+sessionID, "INFO", true))
}

func (s *Server) deviceForSession(ctx context.Context, container *sqlstore.Container, sessionID string) (*waStore.Device, error) {
	if !s.cfg.IsPostgresStore() {
		device, err := container.GetFirstDevice(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return container.NewDevice(), nil
		}
		return device, err
	}
	jidText, err := s.lease.GetSessionDeviceJID(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return container.NewDevice(), nil
	}
	if err != nil {
		return nil, err
	}
	jid, err := types.ParseJID(jidText)
	if err != nil {
		return nil, err
	}
	return container.GetDevice(ctx, jid)
}

func (s *Server) saveSessionDevice(ctx context.Context, sessionID string, jid types.JID) error {
	if !s.cfg.IsPostgresStore() || s.lease == nil {
		return nil
	}
	return s.lease.UpsertSessionDeviceJID(ctx, sessionID, jid.String())
}

func (s *Server) deleteSessionDevice(ctx context.Context, sessionID string) error {
	if !s.cfg.IsPostgresStore() || s.lease == nil {
		return nil
	}
	return s.lease.DeleteSessionDeviceJID(ctx, sessionID)
}

func (s *Server) eventHandler(session *waSession) func(any) {
	return func(evt any) {
		switch v := evt.(type) {
		case *events.Connected:
			s.mu.Lock()
			session.connected = true
			session.qr = ""
			s.mu.Unlock()
			if session.client != nil && session.client.Store != nil && session.client.Store.ID != nil {
				_ = s.saveSessionDevice(context.Background(), session.id, *session.client.Store.ID)
			}
		case *events.Disconnected, *events.LoggedOut:
			session.cancel()
			if session.lease != nil && session.lease.cancel != nil {
				session.lease.cancel()
				_ = s.lease.Release(context.Background(), session.id, session.lease.token)
			}
			s.mu.Lock()
			session.connected = false
			delete(s.sessions, session.id)
			s.mu.Unlock()
		case *events.Message:
			_ = s.forwardInbound(context.Background(), session, v)
		}
	}
}

func (s *Server) forwardInbound(ctx context.Context, session *waSession, evt *events.Message) error {
	if s.cfg.WorkerWebhookURL == "" {
		return nil
	}
	payload := inboundMessage{
		SessionID: session.id,
		MessageID: evt.Info.ID,
		From:      stripServer(evt.Info.Sender.String()),
		Chat:      stripServer(evt.Info.Chat.String()),
		Timestamp: evt.Info.Timestamp.UTC().Format(time.RFC3339),
		Text:      messageText(evt.Message),
	}
	if media := mediaFromMessage(evt.Message); media != nil {
		data, err := session.client.Download(ctx, media.downloadable)
		if err != nil {
			return err
		}
		payload.Media = &mediaObject{FileName: media.fileName, MimeType: media.mimeType, Base64: base64.StdEncoding.EncodeToString(data)}
	}
	return webhook.Client{URL: s.cfg.WorkerWebhookURL, ServiceToken: s.cfg.ServiceToken}.ForwardJSON(ctx, payload)
}

func (s *Server) session(sessionID string) (*waSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[sessionID]
	if session == nil {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	return session, nil
}

func (s *Server) runLeaseHeartbeat(ctx context.Context, session *waSession) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	base := s.cfg.LeaseHeartbeat
	if base <= 0 {
		base = 10 * time.Second
	}
	failures := 0
	for {
		wait := sessionpkg.JitterDuration(base, rng)
		if failures > 0 {
			wait = sessionpkg.BackoffDuration(base, failures, rng)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			if s.lease == nil || session.lease == nil {
				return
			}
			alive, err := s.lease.Renew(ctx, session.id, session.lease.token)
			if err != nil {
				failures++
				continue
			}
			failures = 0
			if alive {
				continue
			}
			session.cancel()
			session.client.Disconnect()
			s.mu.Lock()
			delete(s.sessions, session.id)
			s.mu.Unlock()
			return
		}
	}
}

type downloadableMedia struct {
	downloadable whatsmeow.DownloadableMessage
	fileName     string
	mimeType     string
}

func mediaFromMessage(msg *waE2E.Message) *downloadableMedia {
	if img := msg.GetImageMessage(); img != nil {
		return &downloadableMedia{downloadable: img, fileName: "image.jpg", mimeType: defaultString(img.GetMimetype(), "image/jpeg")}
	}
	if video := msg.GetVideoMessage(); video != nil {
		return &downloadableMedia{downloadable: video, fileName: "video.mp4", mimeType: defaultString(video.GetMimetype(), "video/mp4")}
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return &downloadableMedia{downloadable: doc, fileName: defaultString(doc.GetFileName(), "document"), mimeType: defaultString(doc.GetMimetype(), "application/octet-stream")}
	}
	if audio := msg.GetAudioMessage(); audio != nil {
		return &downloadableMedia{downloadable: audio, fileName: "audio.ogg", mimeType: defaultString(audio.GetMimetype(), "audio/ogg")}
	}
	return nil
}

func (session *waSession) mediaMessage(ctx context.Context, data []byte, media *mediaObject, caption string) (*waE2E.Message, error) {
	kind := mediaKind(media.MimeType)
	uploaded, err := session.client.Upload(ctx, data, kind)
	if err != nil {
		return nil, err
	}
	switch kind {
	case whatsmeow.MediaImage:
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String(caption), Mimetype: proto.String(media.MimeType), URL: proto.String(uploaded.URL), DirectPath: proto.String(uploaded.DirectPath), MediaKey: uploaded.MediaKey, FileEncSHA256: uploaded.FileEncSHA256, FileSHA256: uploaded.FileSHA256, FileLength: proto.Uint64(uint64(len(data)))}}, nil
	case whatsmeow.MediaVideo:
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String(caption), Mimetype: proto.String(media.MimeType), URL: proto.String(uploaded.URL), DirectPath: proto.String(uploaded.DirectPath), MediaKey: uploaded.MediaKey, FileEncSHA256: uploaded.FileEncSHA256, FileSHA256: uploaded.FileSHA256, FileLength: proto.Uint64(uint64(len(data)))}}, nil
	case whatsmeow.MediaAudio:
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String(media.MimeType), URL: proto.String(uploaded.URL), DirectPath: proto.String(uploaded.DirectPath), MediaKey: uploaded.MediaKey, FileEncSHA256: uploaded.FileEncSHA256, FileSHA256: uploaded.FileSHA256, FileLength: proto.Uint64(uint64(len(data)))}}, nil
	default:
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Title: proto.String(media.FileName), FileName: proto.String(media.FileName), Caption: proto.String(caption), Mimetype: proto.String(defaultString(media.MimeType, "application/octet-stream")), URL: proto.String(uploaded.URL), DirectPath: proto.String(uploaded.DirectPath), MediaKey: uploaded.MediaKey, FileEncSHA256: uploaded.FileEncSHA256, FileSHA256: uploaded.FileSHA256, FileLength: proto.Uint64(uint64(len(data)))}}, nil
	}
}

func (s *Server) fetchMedia(ctx context.Context, media *mediaObject) ([]byte, error) {
	if media.Base64 != "" {
		return base64.StdEncoding.DecodeString(media.Base64)
	}
	if media.URL == "" {
		return nil, errors.New("media.url or media.base64 is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, media.URL, nil)
	if err != nil {
		return nil, err
	}
	if s.cfg.ServiceToken != "" {
		req.Header.Set("X-Service-Token", s.cfg.ServiceToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("media fetch returned %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 50<<20))
}

func normalizePhone(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "+")
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, value)
}

func normalizeSessionID(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return -1
	}, value)
	return strings.ToLower(value)
}

func (session *waSession) phoneNumber() string {
	if session.client != nil && session.client.Store != nil && session.client.Store.ID != nil {
		return normalizePhone(session.client.Store.ID.User)
	}
	return ""
}

func phoneToJID(value string) (types.JID, error) {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "@") {
		return types.ParseJID(value)
	}
	phone := normalizePhone(value)
	if phone == "" {
		return types.JID{}, errors.New("invalid phone number")
	}
	return types.ParseJID(phone + "@s.whatsapp.net")
}

func stripServer(value string) string {
	return strings.TrimSuffix(strings.TrimSuffix(value, "@s.whatsapp.net"), "@lid")
}

func messageText(msg *waE2E.Message) string {
	if msg.GetConversation() != "" {
		return msg.GetConversation()
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	if img := msg.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	if video := msg.GetVideoMessage(); video != nil {
		return video.GetCaption()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetCaption()
	}
	return ""
}

func mediaKind(mimeType string) whatsmeow.MediaType {
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		mediaType = mimeType
	}
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return whatsmeow.MediaImage
	case strings.HasPrefix(mediaType, "video/"):
		return whatsmeow.MediaVideo
	case strings.HasPrefix(mediaType, "audio/"):
		return whatsmeow.MediaAudio
	default:
		return whatsmeow.MediaDocument
	}
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
