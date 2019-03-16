// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package amass

import (
	"math/rand"
	"strings"
	"sync"

	"github.com/OWASP/Amass/amass/core"
	"github.com/OWASP/Amass/amass/utils"
	"github.com/miekg/dns"
)

type lenDist struct {
	Count float64
	Freq  float64
}

type markovModel struct {
	sync.Mutex
	TotalLabels int
	Ngrams      map[string]map[rune]*lenDist
}

// MarkovService is the Service that perform DNS name guessing using markov chain models.
type MarkovService struct {
	core.BaseService

	ngramSize int
	numNames  int
	model     *markovModel
	subsLock  sync.Mutex
	subs      map[string]*core.Request
	inFilter  *utils.StringFilter
	outFilter *utils.StringFilter
}

// NewMarkovService returns he object initialized, but not yet started.
func NewMarkovService(config *core.Config, bus *core.EventBus) *MarkovService {
	m := &MarkovService{
		ngramSize: 3,
		numNames:  10000,
		subs:      make(map[string]*core.Request),
		inFilter:  utils.NewStringFilter(),
		outFilter: utils.NewStringFilter(),
		model:     &markovModel{Ngrams: make(map[string]map[rune]*lenDist)},
	}

	m.BaseService = *core.NewBaseService(m, "Markov Model", config, bus)
	return m
}

// OnStart implements the Service interface
func (m *MarkovService) OnStart() error {
	m.BaseService.OnStart()

	if m.Config().Alterations {
		m.Bus().Subscribe(core.NameResolvedTopic, m.SendRequest)
		go m.processRequests()
	}
	return nil
}

func (m *MarkovService) processRequests() {
	for {
		select {
		case <-m.PauseChan():
			<-m.ResumeChan()
		case <-m.Quit():
			return
		case req := <-m.RequestChan():
			go m.trainModel(req)
		}
	}
}

func (m *MarkovService) correctRecordTypes(req *core.Request) bool {
	var ok bool
	for _, r := range req.Records {
		t := uint16(r.Type)

		if t == dns.TypeTXT || t == dns.TypeA || t == dns.TypeAAAA || t == dns.TypeCNAME {
			ok = true
			break
		}
	}
	return ok
}

func (m *MarkovService) trainModel(req *core.Request) {
	if !m.correctRecordTypes(req) ||
		m.inFilter.Duplicate(req.Name) ||
		!m.Config().IsDomainInScope(req.Name) {
		return
	}

	parts := strings.SplitN(req.Name, ".", 2)
	if len(parts) != 2 {
		return
	}
	label := []rune(parts[0] + ".")
	m.outFilter.Duplicate(req.Name)

	for i, char := range label {
		if i-m.ngramSize < 0 {
			var ngram string

			for j := 0; j < abs(i-m.ngramSize); j++ {
				ngram += "`"
			}
			ngram += string(label[0:i])
			m.updateModel(ngram, char)
		} else {
			m.updateModel(string(label[i-m.ngramSize:i]), char)
		}
	}

	m.subsLock.Lock()
	if _, ok := m.subs[parts[1]]; !ok {
		m.subs[parts[1]] = &core.Request{
			Name:   parts[1],
			Domain: req.Domain,
		}
	}
	m.subsLock.Unlock()
	m.updateTotal()
}

func abs(val int) int {
	if val < 0 {
		return -val
	}
	return val
}

func (m *MarkovService) updateModel(ngram string, char rune) {
	m.model.Lock()
	defer m.model.Unlock()

	if _, ok := m.model.Ngrams[ngram]; !ok {
		m.model.Ngrams[ngram] = make(map[rune]*lenDist)
	}
	if _, ok := m.model.Ngrams[ngram][char]; !ok {
		m.model.Ngrams[ngram][char] = new(lenDist)
	}
	m.model.Ngrams[ngram][char].Count++
}

func (m *MarkovService) updateTotal() {
	m.model.Lock()
	m.model.TotalLabels++
	total := m.model.TotalLabels
	m.model.Unlock()

	if (total % 50) == 0 {
		m.updateFrequencies()
		go m.generateNames()
	}
}

func (m *MarkovService) updateFrequencies() {
	m.model.Lock()
	defer m.model.Unlock()

	for ngram := range m.model.Ngrams {
		var total float64

		for char := range m.model.Ngrams[ngram] {
			total += m.model.Ngrams[ngram][char].Count
		}
		for _, ld := range m.model.Ngrams[ngram] {
			ld.Freq = ld.Count / total
		}
	}
}

func (m *MarkovService) generateNames() {
	for i := 0; i < m.numNames; i++ {
		label := m.generateLabel()

		m.subsLock.Lock()
		for _, sub := range m.subs {
			go m.sendGeneratedName(label+"."+sub.Name, sub.Domain)
		}
		m.subsLock.Unlock()
	}
}

func (m *MarkovService) generateLabel() string {
	var result string

	for i := 0; i < m.ngramSize; i++ {
		result += "`"
	}

	max := maxDNSLabelLen + m.ngramSize
	for i := 0; i < max; i++ {
		char := m.generateChar(result[i : i+m.ngramSize])

		if char == "." {
			break
		}
		result += char
	}
	if label := strings.Trim(result, "`"); len(label) > 0 && len(label) <= maxDNSLabelLen {
		return label
	}
	return m.generateLabel()
}

func (m *MarkovService) generateChar(ngram string) string {
	m.model.Lock()
	if chars, ok := m.model.Ngrams[ngram]; ok {
		r := rand.Float64()

		var accum float64
		for char, ld := range chars {
			accum += ld.Freq

			if r <= accum {
				m.model.Unlock()
				return string(char)
			}
		}
	}
	m.model.Unlock()

	chars := []rune(ngram)
	l := len(chars)
	if l-1 < 0 {
		return "."
	}
	return m.generateChar(string(chars[:l-1]))
}

func (m *MarkovService) sendGeneratedName(name, domain string) {
	name = strings.Trim(name, "-")
	if name == "" || m.outFilter.Duplicate(name) {
		return
	}

	re := m.Config().DomainRegex(domain)
	if re == nil || !re.MatchString(name) {
		return
	}

	m.Bus().Publish(core.NewNameTopic, &core.Request{
		Name:   name,
		Domain: domain,
		Tag:    core.ALT,
		Source: m.String(),
	})
}
