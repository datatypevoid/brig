package endpoints

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
	"github.com/sahib/brig/catfs"
	"github.com/sahib/brig/events"
	"github.com/sahib/brig/gateway/db"
	"github.com/sahib/config"
)

type State struct {
	fs     *catfs.FS
	cfg    *config.Config
	ev     *events.Listener
	evHdl  *EventsHandler
	store  *sessions.CookieStore
	userDb *db.UserDatabase
}

func readOrInitKeyFromConfig(cfg *config.Config, keyName string, keyLen int) ([]byte, error) {
	keyStr := cfg.String(keyName)
	if keyStr == "" {
		keyData := securecookie.GenerateRandomKey(keyLen)
		cfg.SetString(keyName, base64.StdEncoding.EncodeToString(keyData))
		return keyData, nil
	}

	return base64.StdEncoding.DecodeString(keyStr)
}

func NewState(
	fs *catfs.FS,
	cfg *config.Config,
	evHdl *EventsHandler,
	userDb *db.UserDatabase,
) (*State, error) {
	authKey, err := readOrInitKeyFromConfig(cfg, "auth.session-authentication-key", 64)
	if err != nil {
		return nil, err
	}

	encKey, err := readOrInitKeyFromConfig(cfg, "auth.session-encryption-key", 32)
	if err != nil {
		return nil, err
	}

	// Generated here, but used by the server:
	_, err = readOrInitKeyFromConfig(cfg, "auth.session-csrf-key", 32)
	if err != nil {
		return nil, err
	}

	return &State{
		fs:     fs,
		cfg:    cfg,
		evHdl:  evHdl,
		store:  sessions.NewCookieStore(authKey, encKey),
		userDb: userDb,
	}, nil
}

func (s *State) Close() error {
	s.evHdl.Shutdown()
	return s.userDb.Close()
}

func (s *State) SetEventListener(ev *events.Listener) {
	s.ev = ev
	s.evHdl.SetEventListener(ev)
}

func (s *State) publishFsEvent(req *http.Request) {
	if s.evHdl != nil {
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()

		s.evHdl.Notify("fs", ctx)
	}

	if s.ev == nil {
		return
	}

	log.Debugf("publishing fs event from gateway")
	ev := events.Event{
		Type: events.FsEvent,
	}

	if err := s.ev.PublishEvent(ev); err != nil {
		log.Warningf("failed to publish filesystem change event: %v", err)
	}
}

func prefixRoot(nodePath string) string {
	if strings.HasPrefix(nodePath, "/") {
		return nodePath
	}

	return "/" + nodePath
}

func buildFolderCache(folders []string) map[string]bool {
	folderCache := make(map[string]bool)
	for _, folder := range folders {
		folderCache[prefixRoot(path.Clean(folder))] = true
	}

	return folderCache
}

func (s *State) folderCacheForUser(w http.ResponseWriter, r *http.Request) (map[string]bool, error) {
	name := getUserName(s.store, w, r)
	if name == "" {
		return nil, fmt.Errorf("no user")
	}

	user, err := s.userDb.Get(name)
	if err != nil {
		return nil, fmt.Errorf("validate: user db: %v", err)
	}

	return buildFolderCache(user.Folders), nil
}

func (s *State) pathIsVisible(nodePath string, w http.ResponseWriter, r *http.Request) bool {
	nodePath = prefixRoot(path.Clean(nodePath))
	if s.validatePath(nodePath, w, r) {
		return true
	}

	folderCache, err := s.folderCacheForUser(w, r)
	if err != nil {
		log.Debugf("failed to build folder cache: %v", err)
		return false
	}

	// Go over all folders, and see if we have some allowed folder
	// that we need to display "on the way". This could be probably
	// made faster if we ever need to.
	for folder, isValid := range folderCache {
		if !isValid {
			continue
		}

		// Example case:
		// folder   = /nested/something
		// nodePath = /nested
		// (also handles if folder == nodePath)
		//
		// Other case (folder = /nested, nodePath = /nested/something)
		// is already handled by calling validatePath() above.
		if strings.HasPrefix(folder, nodePath) {
			return true
		}
	}

	// There is no valid prefix at all.
	return false
}

func (s *State) validatePath(nodePath string, w http.ResponseWriter, r *http.Request) bool {
	folderCache, err := s.folderCacheForUser(w, r)
	if err != nil {
		log.Debugf("could not build folder cache: %v", err)
		return false
	}

	curr := prefixRoot(nodePath)

	for curr != "" {
		if folderCache[curr] {
			return true
		}

		next := path.Dir(curr)
		if curr == "/" && next == curr {
			// We've gone up too much:
			break
		}

		curr = next
	}

	// No fitting path found:
	return false
}

//////////////////////

func jsonify(w http.ResponseWriter, statusCode int, data interface{}) {
	w.WriteHeader(statusCode)
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Warningf("failed to encode json: %v", err)
		w.Write([]byte(
			"{\"success\": false, \"message\": \"failed to encode json response\"}",
		))
		w.WriteHeader(500)
		return
	}
}

func jsonifyErrf(w http.ResponseWriter, statusCode int, format string, data ...interface{}) {
	msg := fmt.Sprintf(format, data...)
	success := false
	if statusCode >= 200 && statusCode < 400 {
		success = true
	} else {
		log.Debugf("failed to respond: %v", msg)
	}

	jsonify(w, statusCode, struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}{
		Success: success,
		Message: msg,
	})
}

func jsonifySuccess(w http.ResponseWriter) {
	jsonifyErrf(w, http.StatusOK, "success")
}
