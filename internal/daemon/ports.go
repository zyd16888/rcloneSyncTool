package daemon

import (
	"fmt"
	"net"
	"sync"
)

type PortManager struct {
	start int
	end   int

	mu    sync.Mutex
	inUse map[int]struct{}
}

func NewPortManager(start, end int) *PortManager {
	if start <= 0 {
		start = 55720
	}
	if end < start {
		end = start
	}
	return &PortManager{
		start: start,
		end:   end,
		inUse: map[int]struct{}{},
	}
}

func (p *PortManager) SetRange(start, end int) {
	if start <= 0 {
		start = p.start
	}
	if end < start {
		end = start
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.start = start
	p.end = end
}

func (p *PortManager) Acquire() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for port := p.start; port <= p.end; port++ {
		if _, ok := p.inUse[port]; ok {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		p.inUse[port] = struct{}{}
		return port, nil
	}
	return 0, fmt.Errorf("no free rc port in range %d-%d", p.start, p.end)
}

func (p *PortManager) Release(port int) {
	if port <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inUse, port)
}
