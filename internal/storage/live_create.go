package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ImportKeyPrefix помечает строки matches, созданные из фикстур источника.
//
// Пайплайн использует слаг-подобные ключи ("fils_barcelona_f_2026"), так что
// наш префикс с ними не пересекается — и это не только про идемпотентность:
// по нему видно, какие строки наши, и по нему же работает их уборка.
const ImportKeyPrefix = "livetennisapi:"

// CreateOutcome — почему строка не создана. Нужен, чтобы отличать «уже есть»
// (норма) от «не смогли» (сигнал): в счётчиках прогона это разные величины.
type CreateOutcome int

const (
	CreateDone CreateOutcome = iota
	// Матч этих двоих в этом розыгрыше уже есть — наш или пайплайна.
	CreateExists
	// round_code источника отсутствует в нашей таблице rounds.
	CreateUnknownRound
)

// MatchDraft — то, из чего создаётся строка матча. Все id уже разрешены:
// функция ничего не угадывает.
type MatchDraft struct {
	EditionID   int64
	RoundCode   string
	ScheduledAt time.Time
	PlayerIDs   [2]int64
	ExternalKey string
}

// CreateMatchFromFixture создаёт scheduled-матч и двух участников.
//
// Только INSERT, никогда UPDATE: строку, которой владеет пайплайн, этот сервис
// не трогает вообще.
//
// Проверки идут в этом порядке и каждая нужна:
//
//  1. round_code должен ЛЕЖАТЬ В rounds. Это внешний ключ, а словарь источника
//     нашему не подмножество: у него есть BR, голый Q, Q4 и ER, которых у нас
//     нет. Проверка «не пустой» тут не годится — on conflict нарушение внешнего
//     ключа не поглощает, и на такой строке падал бы весь оператор.
//
//  2. Матча этих двоих в этом розыгрыше быть не должно. Сверяемся по паре
//     игроков и НЕ по раунду: наши собственные словари несогласованы
//     (roland_garros_2026 использует R128/R64, wimbledon_2026 — R1/R2 при той
//     же сетке), а draw_size у всех 25 розыгрышей NULL. Двое встречаются в
//     сетке на вылет не больше одного раза; исключение — круговой групповой
//     этап на ATP Finals, один турнир в году.
//
//  3. discipline задаётся ЯВНО, а не берётся из умолчания колонки. Умолчание
//     живёт в схеме, которой владеет другой репозиторий: полагаться на него —
//     значит зависеть от чужого решения, которое могут поменять, не зная про нас.
//
//  4. import_key даёт идемпотентность. do update, а не do nothing: последний
//     не возвращает строку, и на повторном прогоне id для вставки участников
//     взять было бы неоткуда.
//
// Матч и оба участника пишутся В ОДНОЙ транзакции: падение между ними оставило
// бы матч без участников — невидимый для поиска по игрокам и для всех
// экранов, но занимающий место в сетке.
func CreateMatchFromFixture(ctx context.Context, pool *pgxpool.Pool,
	d MatchDraft) (int64, CreateOutcome, error) {

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, CreateDone, err
	}
	defer tx.Rollback(ctx)

	var roundExists bool
	if err := tx.QueryRow(ctx,
		`select exists (select 1 from rounds where code = $1)`,
		d.RoundCode).Scan(&roundExists); err != nil {
		return 0, CreateDone, err
	}
	if !roundExists {
		return 0, CreateUnknownRound, nil
	}

	var existing int64
	err = tx.QueryRow(ctx, `
		select m.id from matches m
		where m.edition_id = $1
		  and exists (select 1 from match_participants mp
		              where mp.match_id = m.id and mp.player_id = $2)
		  and exists (select 1 from match_participants mp
		              where mp.match_id = m.id and mp.player_id = $3)
		limit 1`,
		d.EditionID, d.PlayerIDs[0], d.PlayerIDs[1]).Scan(&existing)
	if err == nil {
		return existing, CreateExists, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, CreateDone, err
	}

	var matchID int64
	if err := tx.QueryRow(ctx, `
		insert into matches (edition_id, round_code, scheduled_at, status,
		                     discipline, import_key, metadata)
		values ($1, $2, $3, 'scheduled', 'singles', $4, $5::jsonb)
		on conflict (import_key) do update set import_key = excluded.import_key
		returning id`,
		d.EditionID, d.RoundCode, d.ScheduledAt, ImportKeyPrefix+d.ExternalKey,
		fmt.Sprintf(`{"source":"livetennisapi","external_key":%q,"created_by":"live-schedule"}`,
			d.ExternalKey)).Scan(&matchID); err != nil {
		return 0, CreateDone, err
	}

	for side, playerID := range d.PlayerIDs {
		if _, err := tx.Exec(ctx, `
			insert into match_participants (match_id, side, slot, player_id)
			values ($1, $2, 1, $3)
			on conflict do nothing`,
			matchID, side+1, playerID); err != nil {
			return 0, CreateDone, err
		}
	}
	return matchID, CreateDone, tx.Commit(ctx)
}

// ResolveEditions — id турниров источника -> наши розыгрыши.
//
// Берутся ТОЛЬКО подтверждённые привязки (confirmed_at is not null).
//
// Это единственное место во всей фиче, где догадка приводила бы к записи строк
// в таблицу, которой владеет другой репозиторий. Везде остальным месте
// неподтверждённая привязка — это запись в очередь ревью, а не рабочий факт;
// здесь должно быть так же. Половина сида выведена из каталога /tournaments:
// турнир он называет верно, но роль каждого id (основная сетка, квалификация,
// пара) не сообщает, а именно она решает, в какую сетку попадёт матч. Заголовок
// db/live_edition_ids.sql называет цену прямо: неверно угаданный розыгрыш
// создаёт матч в чужой сетке.
//
// Неподтверждённый id уходит в live_unmatched как edition_unmapped — то есть в
// ту же очередь, из которой человек его и подтвердит.
func ResolveEditions(ctx context.Context, pool *pgxpool.Pool, source string,
	keys []string) (map[string]int64, error) {

	rows, err := pool.Query(ctx, `
		select external_key, entity_id from external_ids
		where source = $1 and entity_type = 'edition' and external_key = any($2)
		  and confirmed_at is not null`,
		source, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64, len(keys))
	for rows.Next() {
		var (
			key string
			id  int64
		)
		if err := rows.Scan(&key, &id); err != nil {
			return nil, err
		}
		out[key] = id
	}
	return out, rows.Err()
}

// RetireCreatedMatches убирает наши созданные строки, которые так и остались
// scheduled заметно позже назначенного времени.
//
// Без уборки такая строка живёт вечно с датой в прошлом и становится у обоих
// игроков «следующим матчем» навсегда — ingest возвращает матч в прежний
// статус после окончания, а перевести его в терминальный некому: результат
// принадлежит пайплайну, и придумывать его мы не будем.
//
// Трогаем ТОЛЬКО свои строки (по префиксу import_key) и только те, что никто
// не переводил в live.
func RetireCreatedMatches(ctx context.Context, pool *pgxpool.Pool,
	olderThan time.Time) (int64, error) {

	tag, err := pool.Exec(ctx, `
		update matches set status = 'cancelled'
		where import_key like $1
		  and status = 'scheduled'
		  and scheduled_at is not null
		  and scheduled_at < $2
		  and not exists (select 1 from live_flags f where f.match_id = matches.id)`,
		ImportKeyPrefix+"%", olderThan)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
