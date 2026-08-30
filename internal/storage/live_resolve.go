package storage

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Ленивое разрешение игроков.
//
// Сид из db/live_external_ids.sql покрывает 101 из 102 отслеживаемых игроков, но
// он снят с рейтингового среза и полным быть не может: у источника есть
// раздвоенные записи одного человека (Tabur 175 и 8824, Misolic 13409 и 14985),
// а игроки вне первой полутора сотни в срез не попадают вовсе — Shelbayh был на
// корте в момент замера и в файле отсутствует.
//
// НАСКОЛЬКО ЭТО НАДЁЖНО. Сопоставлять можно только по именам: birth_date и
// country_code у всех 173 наших игроков NULL, так что ни дата рождения, ни
// страна в разрешении не участвуют — их просто нет. Поэтому предикат нарочито
// строгий и отказывается чаще, чем мог бы: неверная привязка тихо покажет
// карточку не того игрока, а это пуш, который не отзовёшь. Лучше не показать
// карточку, чем показать чужую.

var nonAlnum = regexp.MustCompile(`[^a-z0-9 ]+`)

// foldLatin — таблица «буква с диакритикой -> базовая».
//
// Своя таблица, а не golang.org/x/text: он есть в графе модулей только как
// косвенная зависимость, и обращение к нему напрямую перевело бы его в прямые,
// то есть изменило бы go.mod. Весь остальной ingest обходится стандартной
// библиотекой, и разменивать это на пятнадцать строк не стоит. Покрыты
// Latin-1 Supplement и Latin Extended-A — этого хватает на любые имена тура.
var foldLatin = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'ā': "a", 'ă': "a", 'ą': "a",
	'ç': "c", 'ć': "c", 'č': "c", 'ĉ': "c", 'ċ': "c",
	'ď': "d", 'đ': "d",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e", 'ē': "e", 'ĕ': "e", 'ė': "e", 'ę': "e", 'ě': "e",
	'ĝ': "g", 'ğ': "g", 'ġ': "g", 'ģ': "g",
	'ĥ': "h", 'ħ': "h",
	'ì': "i", 'í': "i", 'î': "i", 'ï': "i", 'ĩ': "i", 'ī': "i", 'į': "i", 'ı': "i",
	'ĵ': "j", 'ķ': "k",
	'ĺ': "l", 'ļ': "l", 'ľ': "l", 'ł': "l",
	'ñ': "n", 'ń': "n", 'ņ': "n", 'ň': "n",
	'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o", 'ō': "o", 'ŏ': "o", 'ő': "o",
	'ŕ': "r", 'ŗ': "r", 'ř': "r",
	'ś': "s", 'ŝ': "s", 'ş': "s", 'š': "s",
	'ţ': "t", 'ť': "t", 'ŧ': "t",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u", 'ũ': "u", 'ū': "u", 'ŭ': "u", 'ů': "u", 'ű': "u", 'ų': "u",
	'ŵ': "w", 'ý': "y", 'ÿ': "y", 'ŷ': "y",
	'ź': "z", 'ż': "z", 'ž': "z",
	'ß': "ss", 'æ': "ae", 'œ': "oe", 'ð': "d", 'þ': "th",
}

// normalizeName убирает диакритику и регистр: "Sebastián Báez" -> "sebastian baez".
func normalizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if folded, ok := foldLatin[r]; ok {
			b.WriteString(folded)
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(nonAlnum.ReplaceAllString(b.String(), " ")), " ")
}

// ErrNoConfidentMatch — кандидат не один. Отказ, а не выбор наугад.
var ErrNoConfidentMatch = errors.New("no confident player match")

// MatchPlayerByName ищет НАШЕГО игрока по имени от источника.
//
// Кандидат — игрок, у которого совпадает нормализованное полное имя ЛИБО
// нормализованная фамилия. Ровно один кандидат — привязываем; ноль или
// несколько — отказ.
//
// Два ограничения, каждое из которых закрывает дыру в этом «ровно одном»:
//
//  1. Кандидаты ищутся среди ВСЕХ игроков, а не только отслеживаемых. Сужение
//     до отслеживаемых выглядит безопаснее, а на деле слабее: однофамилец,
//     который есть у нас, но не отслеживается, при таком сужении не создаёт
//     неоднозначности — и отказ, ради которого всё затевалось, не срабатывает.
//     На всей базе однофамильцы есть (братья Cerundolo), и пусть лучше
//     сомнительный случай упрётся в отказ.
//
//  2. Ветка «только фамилия» требует у источника минимум ДВА слова. Иначе
//     односложное имя вендора ("Zverev") сопоставляется по фамилии в одиночку —
//     а именно так выглядят его же записи-заглушки, то есть самый бедный
//     данными случай получал бы самое слабое доказательство.
func MatchPlayerByName(ctx context.Context, pool *pgxpool.Pool, vendorName string) (int64, error) {
	full := normalizeName(vendorName)
	if full == "" {
		return 0, ErrNoConfidentMatch
	}
	parts := strings.Fields(full)
	surname := parts[len(parts)-1]

	rows, err := pool.Query(ctx, `
		select p.id, p.display_name, coalesce(p.last_name, '') from players p`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var found []int64
	for rows.Next() {
		var (
			id                int64
			display, lastName string
		)
		if err := rows.Scan(&id, &display, &lastName); err != nil {
			return 0, err
		}
		nd := normalizeName(display)
		nl := normalizeName(lastName)
		switch {
		case nd == full:
			found = append(found, id)
		case len(parts) >= 2 && nl != "" && nl == surname:
			found = append(found, id)
		// наше display_name бывает усечённым: "Andres Burruchaga" против
		// "Roman Andres Burruchaga" у источника. Принимаем, только если наши
		// слова — подмножество и их не меньше двух.
		case len(strings.Fields(nd)) >= 2 && isSubsequenceOfWords(nd, full):
			found = append(found, id)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(found) != 1 {
		return 0, ErrNoConfidentMatch
	}
	return found[0], nil
}

func isSubsequenceOfWords(sub, full string) bool {
	have := map[string]bool{}
	for _, w := range strings.Fields(full) {
		have[w] = true
	}
	for _, w := range strings.Fields(sub) {
		if !have[w] {
			return false
		}
	}
	return true
}

// UpsertPlayerMapping записывает найденную привязку.
//
// confirmed_at остаётся NULL: это машинная догадка, ждущая ревью. И do nothing,
// а не do update — подтверждённую человеком привязку машинная перезаписывать
// не должна никогда.
func UpsertPlayerMapping(ctx context.Context, pool *pgxpool.Pool, source,
	externalKey string, playerID int64) error {

	_, err := pool.Exec(ctx, `
		insert into external_ids (source, entity_type, external_key, entity_id, confirmed_at)
		values ($1, 'player', $2, $3, null)
		on conflict (source, entity_type, external_key) do nothing`,
		source, externalKey, playerID)
	return err
}

// ShouldTryResolve — стоит ли тратить запрос на этот ключ.
//
// Негативный кэш обязателен, а не желателен. Без него неизвестный id
// запрашивался бы каждый цикл вечно, а именно это и происходит с Holger Rune:
// его нет в индексе источника вообще, то есть ответ всегда будет пустым.
// Отступ растёт с числом попыток, потолок — неделя.
func ShouldTryResolve(ctx context.Context, pool *pgxpool.Pool, source,
	externalKey string, now time.Time) (bool, error) {

	var (
		lastTried time.Time
		attempts  int
	)
	err := pool.QueryRow(ctx, `
		select last_tried_at, attempts from live_resolve_attempts
		where source = $1 and external_key = $2`,
		source, externalKey).Scan(&lastTried, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}

	// attempts=1 -> 1 час, 2 -> 2 часа, 3 -> 4 часа и так далее.
	// Сдвиг от attempts-1, а не от attempts: иначе первый отступ уже два часа,
	// то есть первая же неудача стоит вдвое дороже задуманного.
	backoff := time.Duration(1<<min(attempts-1, 7)) * time.Hour
	if backoff > 7*24*time.Hour {
		backoff = 7 * 24 * time.Hour
	}
	return now.Sub(lastTried) >= backoff, nil
}

// RecordResolveAttempt отмечает неудачную попытку.
func RecordResolveAttempt(ctx context.Context, pool *pgxpool.Pool, source,
	externalKey string, now time.Time) error {

	_, err := pool.Exec(ctx, `
		insert into live_resolve_attempts (source, external_key, last_tried_at, attempts)
		values ($1, $2, $3, 1)
		on conflict (source, external_key) do update set
			last_tried_at = excluded.last_tried_at,
			attempts = live_resolve_attempts.attempts + 1`,
		source, externalKey, now)
	return err
}
