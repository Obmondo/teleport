/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package reversetunnel

import "sync"

type agentStore struct {
	agents []*Agent
	mu     sync.RWMutex
}

func newAgentStore() *agentStore {
	return &agentStore{
		agents: make([]*Agent, 0),
	}
}

func (s *agentStore) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.agents)
}

func (s *agentStore) add(agent *Agent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents = append(s.agents, agent)
}

func (s *agentStore) unsafeRemove(agent *Agent) bool {
	for i := range s.agents {
		if s.agents[i] != agent {
			continue
		}
		s.agents = append(s.agents[:i], s.agents[i+1:]...)
		return true
	}

	return false
}

func (s *agentStore) remove(agent *Agent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unsafeRemove(agent)
}

// poplen pops an agent from the store if there are more agents in the store
// than the the given value. The oldest agent is always returned first.
func (s *agentStore) poplen(l int) (*Agent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if l < 0 || len(s.agents) == 0 {
		return nil, false
	}
	if len(s.agents) <= l {
		return nil, false
	}

	agent := s.agents[0]
	s.agents = s.agents[1:]
	return agent, true
}

func (s *agentStore) last() (*Agent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.agents) == 0 {
		return nil, false
	}

	return s.agents[len(s.agents)-1], true
}
