package ops

import (
	"context"
	"errors"
	"sync"
)

type networkRuntime struct {
	manager *Manager
	jarsMu  sync.Mutex
	jars    map[string]*cookieJarState
	wsMu    sync.RWMutex
	ws      map[string]*webSocketSession
	ctx     context.Context
	cancel  context.CancelFunc
}

func (m *Manager) networkState() *networkRuntime {
	m.networkOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		m.network = &networkRuntime{manager: m, jars: make(map[string]*cookieJarState), ws: make(map[string]*webSocketSession), ctx: ctx, cancel: cancel}
	})
	return m.network
}

func (n *networkRuntime) close() {
	n.cancel()
	n.wsMu.Lock()
	sessions := make([]*webSocketSession, 0, len(n.ws))
	for _, session := range n.ws {
		sessions = append(sessions, session)
	}
	n.ws = make(map[string]*webSocketSession)
	n.wsMu.Unlock()
	// Never hold the registry lock across network I/O. Shutdown uses immediate
	// close because the runtime context is already cancelled.
	for _, session := range sessions {
		session.abort(errors.New("service shutdown"))
	}
}
