package discord

import (
	"context"
	"go.kenn.io/msgvault/internal/store"
)

// This explicit positive unit double isolates provenance and rollback. The
// native HTTP parser/SDK tests independently prove actual current authority.
type nativeDiscordUnitAPI struct{ *importerFakeAPI }

func (a nativeDiscordUnitAPI) Me(context.Context) (User, error) {
	return User{ID: "101", Bot: true}, nil
}
func (a nativeDiscordUnitAPI) GuildMember(context.Context, string, string) (NativeGuildMember, error) {
	return NativeGuildMember{User: User{ID: "101", Bot: true}, Roles: []string{}}, nil
}
func (a nativeDiscordUnitAPI) GuildRoles(context.Context, string) ([]NativeGuildRole, error) {
	return []NativeGuildRole{{ID: "200", Permissions: "66560"}}, nil
}
func newNativeDiscordUnitImporter(st *store.Store, api *importerFakeAPI) *Importer {
	return newTestImporter(st, nativeDiscordUnitAPI{api})
}
