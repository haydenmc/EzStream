package main

import "errors"

type ChannelNotification struct {
	Id     string
	Name   string
	IsLive bool
}

type ChannelInfo struct {
	Id      string
	Name    string
	AuthKey string
}

type ChannelStore struct {
	channels []ChannelInfo
}

func NewChannelStore(channels []ChannelInfo) *ChannelStore {
	return &ChannelStore{channels: channels}
}

func (s *ChannelStore) FindById(id string) (*ChannelInfo, error) {
	for _, c := range s.channels {
		if c.Id == id {
			return &c, nil
		}
	}
	return nil, errors.New("could not find channel with specified ID")
}

func (s *ChannelStore) FindByAuthKey(authKey string) (*ChannelInfo, error) {
	for _, c := range s.channels {
		if c.AuthKey == authKey {
			return &c, nil
		}
	}
	return nil, errors.New("could not find channel with specified auth key")
}

func (s *ChannelStore) All() []ChannelInfo {
	return s.channels
}
