// Package livesource — разбор выдачи внешнего источника live-статусов.
//
// Всё, что специфично для конкретного вендора, живёт здесь: смена источника
// должна быть новым файлом в этом пакете, а не переписыванием ingest'а.
// Ниже по течению не должно остаться ни одного места, знающего имена полей
// вендора.
package livesource

import (
	"context"
	"time"
)

// State — что источник говорит про матч.
//
// Значения 'scheduled' здесь намеренно НЕТ. Его никто не порождает: расписание
// живёт в live_schedule, а не в наблюдениях. Если бы оно появилось, запись
// такого наблюдения сдвинула бы last_seen_run_id и молча сбросила счётчик
// пропусков живого матча — то есть карточка не погасла бы никогда.
type State string

const (
	StateOnCourt   State = "on_court"
	StateFinished  State = "finished"
	StateSuspended State = "suspended"
)

// Observation — одна строка live-борта, приведённая к нашим понятиям.
//
// Здесь НЕТ и не может быть ни одного поля со счётом: ни сетов, ни геймов, ни
// очков, ни подающего. Это типовая половина правила 1 из
// docs/live-status-ingest.md — карточка показывает факт присутствия на корте,
// а не счёт. Вторая половина в parse.go: структура разбора не описывает
// объект score вовсе, поэтому пронести счёт дальше физически нечем.
type Observation struct {
	// Идентификатор матча у источника.
	ExternalKey string
	// Идентификаторы игроков у источника; резолвятся через external_ids.
	PlayerKeys [2]string
	// Код раунда уже в нашем словаре (F, SF, QF, R16...). Может быть пустым.
	RoundCode string
	// Идентификатор турнира у источника. На live-борте бывает пустым
	// (в срезе — у 9 строк из 19), поэтому опираться на него на live-пути нельзя.
	TournamentKey string
	ScheduledAt   *time.Time
	State         State
	// Сырое event_status: enum вендора уже дрейфует, поэтому храним как есть
	// для логов и для теста, который ловит появление нового значения.
	EventStatus string
}

// Fixture — предстоящий матч наших игроков: из него строится окно опроса,
// а в Phase 8 — и сама строка matches.
type Fixture struct {
	ExternalKey   string
	PlayerKeys    [2]string
	RoundCode     string
	TournamentKey string
	Tournament    string
	// null означает, что порядок игры ещё не опубликован. Это реальное
	// состояние, а не пробел: такую фикстуру смотрим весь день по EventDate.
	ScheduledAt *time.Time
	EventDate   *time.Time
}

// Board — разобранный live-борт.
type Board struct {
	Observations []Observation
	// Сколько строк вернул источник всего (до фильтрации).
	RowsParsed int
	// Сколько отброшено на разборе: парные разряды, строки без id игроков,
	// нераспознанные статусы.
	RowsSkipped int
	// У источника есть ещё страницы. Каждая страница — ещё один запрос
	// из суточной квоты.
	HasMore bool
	// Нераспознанные значения event_status. Не ошибка разбора, но повод
	// залогировать: спека вендора дрейфует.
	UnknownEventStatuses []string
}

// FixturePage — разобранный борт предстоящих матчей.
type FixturePage struct {
	Fixtures    []Fixture
	RowsParsed  int
	RowsSkipped int
	HasMore     bool
}

// Source — источник live-статусов. Интерфейс существует, чтобы смена вендора
// была новым файлом, а не переписыванием ingest'а.
type Source interface {
	Name() string
	PollLive(ctx context.Context) (Board, error)
	Fixtures(ctx context.Context, playerKeys []string) (FixturePage, error)
}
