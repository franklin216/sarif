// Copyright (C) 2014 Constantin Schomburg <me@cschomburg.com>
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Service store provides a key-value store to the sarif network.
package store

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/sarifsystems/sarif/pkg/mapq"
	"github.com/sarifsystems/sarif/sarif"
	"github.com/sarifsystems/sarif/services"
)

var Module = &services.Module{
	Name:        "store",
	Version:     "1.0",
	NewInstance: NewService,
}

type Config struct {
	Driver string
	Path   string
}

type Dependencies struct {
	Config services.Config
	Client *sarif.Client
}

type Service struct {
	Config services.Config
	Cfg    Config
	Store  Store
	*sarif.Client
}

func NewService(deps *Dependencies) *Service {
	s := &Service{
		Config: deps.Config,
		Client: deps.Client,
	}
	return s
}

func (s *Service) Enable() (err error) {
	if _, ok := drivers["bolt"]; ok {
		s.Cfg.Driver = "bolt"
		s.Cfg.Path = s.Config.Dir() + "/" + "store.bolt.db"
	} else if _, ok := drivers["memory"]; ok {
		s.Cfg.Driver = "mem"
	}
	s.Config.Get(&s.Cfg)

	if s.Cfg.Driver == "" {
		return errors.New("Store: no database driver set in config!")
	}
	drv, ok := drivers[s.Cfg.Driver]
	if !ok {
		return errors.New("Store: no driver with name '" + s.Cfg.Driver + "' found!")
	}

	if s.Store, err = drv.Open(s.Cfg.Path); err != nil {
		return err
	}

	s.Subscribe("store/put", "", s.handlePut)
	s.Subscribe("store/get", "", s.handleGet)
	s.Subscribe("store/del", "", s.handleDel)
	s.Subscribe("store/scan", "", s.handleScan)
	return nil
}

func (s *Service) Disable() error { return nil }

func parseAction(prefix, action string) (col, key string) {
	if !strings.HasPrefix(action, prefix) {
		return "", ""
	}
	colkey := strings.TrimPrefix(action, prefix)
	parts := strings.SplitN(colkey, "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func (s *Service) handlePut(msg sarif.Message) {
	collection, key := parseAction("store/put/", msg.Action)
	if collection == "" {
		s.ReplyBadRequest(msg, errors.New("No collection specified."))
		return
	}

	if len(msg.Payload.Raw) == 0 && msg.Text != "" {
		v, _ := json.Marshal(msg.Text)
		msg.Payload.Raw = json.RawMessage(v)
	}
	var p interface{}
	if err := msg.DecodePayload(&p); err != nil {
		s.ReplyBadRequest(msg, err)
		return
	}
	// TODO: maybe a JSON payload consistency check
	doc, err := s.Store.Put(&Document{
		Collection: collection,
		Key:        key,
		Value:      msg.Payload.Raw,
	})
	if err != nil {
		s.ReplyInternalError(msg, err)
		return
	}

	doc.Value = nil
	reply := sarif.CreateMessage("store/updated/"+doc.Collection+"/"+doc.Key, doc)
	s.Reply(msg, reply)

	pub := sarif.CreateMessage("store/updated/"+doc.Collection+"/"+doc.Key, doc)
	s.Publish(pub)
}

func (s *Service) handleGet(msg sarif.Message) {
	collection, key := parseAction("store/get/", msg.Action)
	if collection == "" || key == "" {
		s.ReplyBadRequest(msg, errors.New("No collection or key specified."))
		return
	}

	doc, err := s.Store.Get(collection, key)
	if err != nil {
		s.ReplyInternalError(msg, err)
		return
	} else if doc == nil {
		s.Reply(msg, sarif.Message{
			Action: "err/notfound",
			Text:   "Document " + collection + "/" + key + " not found.",
		})
		return
	}

	raw := json.RawMessage(doc.Value)
	reply := sarif.CreateMessage("store/retrieved/"+doc.Collection+"/"+doc.Key, nil)
	reply.Payload.Encode(&raw)
	s.Reply(msg, reply)
}

func (s *Service) handleDel(msg sarif.Message) {
	collection, key := parseAction("store/del/", msg.Action)
	if collection == "" || key == "" {
		s.ReplyBadRequest(msg, errors.New("No collection or key specified."))
		return
	}
	if err := s.Store.Del(collection, key); err != nil {
		s.ReplyInternalError(msg, err)
		return
	}

	s.Reply(msg, sarif.CreateMessage("store/deleted/"+collection+"/"+key, nil))
	s.Publish(sarif.CreateMessage("store/deleted/"+collection+"/"+key, nil))
}

type scanMessage struct {
	Prefix  string `json:"prefix"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Only    string `json:"only"`
	Limit   int    `json:"limit"`
	Reverse bool   `json:"reverse"`

	Filter map[string]interface{} `json:"filter"`
}

func (s *Service) handleScan(msg sarif.Message) {
	collection, prefix := parseAction("store/scan/", msg.Action)
	if collection == "" {
		s.ReplyBadRequest(msg, errors.New("No collection specified."))
		return
	}

	var p scanMessage
	if err := msg.DecodePayload(&p); err != nil {
		s.ReplyBadRequest(msg, err)
		return
	}
	if p.Start == "" && p.End == "" {
		if p.Prefix == "" {
			p.Prefix = prefix
		}
		if p.Prefix != "" {
			p.Start = prefix
			p.End = prefix + "~~~~~" // oh god, what a hack
		}
	}
	if p.Limit == 0 {
		p.Limit = 100
	}

	cursor, err := s.Store.Scan(collection, p.Start, p.End, p.Reverse)
	if err != nil {
		s.ReplyInternalError(msg, err)
		return
	}
	defer cursor.Close()

	keys := make([]string, 0)
	docs := make(map[string]*json.RawMessage)
	values := make([]*json.RawMessage, 0)
	for doc := cursor.Next(); doc != nil && p.Limit > 0; doc = cursor.Next() {
		if p.Filter != nil {
			var object map[string]interface{}
			if err := json.Unmarshal(doc.Value, &object); err != nil {
				continue
			}
			if mapq.M(object).MatchesNot(mapq.Filter(p.Filter)) {
				continue
			}
		}

		p.Limit--
		if p.Only == "keys" {
			keys = append(keys, doc.Key)
		} else if p.Only == "values" {
			raw := json.RawMessage(doc.Value)
			values = append(values, &raw)
		} else {
			raw := json.RawMessage(doc.Value)
			docs[doc.Key] = &raw
		}
	}

	reply := sarif.CreateMessage("store/scanned/"+collection, nil)
	if p.Only == "keys" {
		reply.Payload.Encode(keys)
	} else if p.Only == "values" {
		reply.Payload.Encode(values)
	} else {
		reply.Payload.Encode(docs)
	}
	s.Reply(msg, reply)
}
