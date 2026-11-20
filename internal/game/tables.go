package game

// tableVersion is the game data table version every generated table (*_gen.go) and every list
// still copied from a table by hand was taken from: tables 40413, bundled with the 1.0.370 client
// (iOS 918, Android 1911; live servers were at 40460 when it was taken). Each generated file states
// the version it was generated from in its header, and TestGeneratedTablesShareTableVersion holds
// every header to this constant, so a regeneration that skips a generator (the mail groups need
// LASTWAR_LUA as well) fails a plain go test. Regenerate with go generate ./internal/game, with
// LASTWAR_TABLES and LASTWAR_TABLE_VERSION set, and LASTWAR_LUA and LASTWAR_LUA_VERSION for the
// mail groups; then move this constant to the new version (MASTER B73, cov-tables/F14).
const tableVersion = "40413"
