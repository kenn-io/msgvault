package api

import (
	"context"
	"strings"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/taskclient"
)

type personAgendaBackend struct {
	config config.TaskIntegrationConfig
	people personagenda.IdentityStore
}

func newPersonAgendaBackend(cfg *config.Config, messageStore MessageStore) PersonAgendaOperations {
	people, ok := messageStore.(personagenda.IdentityStore)
	if !ok || cfg == nil {
		return nil
	}
	return &personAgendaBackend{config: cfg.Integrations.Kata, people: people}
}

func (b *personAgendaBackend) integrationConfig() taskclient.IntegrationConfig {
	return taskclient.IntegrationConfig{Enabled: b.config.Enabled, Endpoint: b.config.Endpoint, APIKey: b.config.APIKey, DefaultProject: b.config.DefaultProject}
}

func (b *personAgendaBackend) service(ctx context.Context) (personagenda.Service, error) {
	if !b.config.Enabled {
		return personagenda.Service{}, taskclient.ErrUnreachable
	}
	client, err := taskclient.ConnectKata(ctx, b.integrationConfig())
	if err != nil {
		return personagenda.Service{}, err
	}
	return personagenda.Service{Tasks: client, People: b.people, Project: strings.TrimSpace(b.config.DefaultProject)}, nil
}

func (b *personAgendaBackend) List(ctx context.Context, personID int64) (personagenda.Result, error) {
	service, err := b.service(ctx)
	if err != nil {
		return personagenda.Result{}, err
	}
	return service.List(ctx, personID)
}

func (b *personAgendaBackend) Create(ctx context.Context, personID int64, key string, input personagenda.CreateInput) (personagenda.Item, error) {
	service, err := b.service(ctx)
	if err != nil {
		return personagenda.Item{}, err
	}
	return service.Create(ctx, personID, key, input)
}

func (b *personAgendaBackend) Link(ctx context.Context, personID int64, taskID, list string) (personagenda.Item, error) {
	service, err := b.service(ctx)
	if err != nil {
		return personagenda.Item{}, err
	}
	return service.Link(ctx, personID, taskID, list)
}

func (b *personAgendaBackend) Update(ctx context.Context, personID int64, taskID string, input personagenda.UpdateInput) (personagenda.Item, error) {
	service, err := b.service(ctx)
	if err != nil {
		return personagenda.Item{}, err
	}
	return service.Update(ctx, personID, taskID, input)
}

func (b *personAgendaBackend) Unlink(ctx context.Context, personID int64, taskID string) (personagenda.Item, error) {
	service, err := b.service(ctx)
	if err != nil {
		return personagenda.Item{}, err
	}
	return service.Unlink(ctx, personID, taskID)
}
