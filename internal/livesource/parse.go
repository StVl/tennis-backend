package livesource

import (
	"encoding/json"
	"fmt"
	"time"
)

// Структуры разбора описывают ТОЛЬКО то, что нам можно знать.
//
// Обратите внимание, чего здесь нет: объекта score. Ответ вендора содержит
// сеты, геймы, очки, подающего и вероятности победы — и ни одно из этих полей
// не описано ниже, поэтому пронести счёт дальше разбора нечем. Это не
// упущение, а правило 1 из docs/live-status-ingest.md, выраженное типами:
// карточка показывает присутствие на корте, а не счёт. Следующий читатель
// резонно удивится, куда делись самые интересные поля — вот ответ.
type wireBoard struct {
	Data []wireMatch `json:"data"`
	Meta wireMeta    `json:"meta"`
}

type wireMeta struct {
	HasMore bool `json:"has_more"`
}

type wireMatch struct {
	ID            flexKey    `json:"id"`
	Status        string     `json:"status"`
	EventStatus   string     `json:"event_status"`
	Draw          string     `json:"draw"`
	IsDoubles     bool       `json:"is_doubles"`
	IsQualifying  bool       `json:"is_qualifying"`
	RoundCode     string     `json:"round_code"`
	ScheduledTime *time.Time `json:"scheduled_time"`
	TournamentID  flexKey    `json:"tournament_id"`
	Tournament    string     `json:"tournament"`
	Players       struct {
		P1 wirePlayer `json:"p1"`
		P2 wirePlayer `json:"p2"`
	} `json:"players"`
}

type wirePlayer struct {
	ID            flexKey `json:"id"`
	IsDoublesTeam bool    `json:"is_doubles_team"`
}

// flexKey — идентификатор источника, который приходит то числом, то строкой:
// у матча id это число (177341), у турнира tournament_id — строка ("14188").
type flexKey string

func (k *flexKey) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		*k = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*k = flexKey(str)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*k = flexKey(n.String())
	return nil
}

// ParseBoard разбирает ответ GET /matches?status=live.
//
// Пустой data[] — НЕ ошибка: решение о том, нормально ли это, принимает
// вызывающий (у него есть расписание). А вот тело, которое не разбирается —
// ошибка, и именно ошибка, а не «ноль матчей»: страница 5xx от прокси,
// оборванный ответ и пустой борт должны различаться, иначе сбой сети погасит
// карточки у всех пользователей.
func ParseBoard(body []byte) (Board, error) {
	var wire wireBoard
	if err := json.Unmarshal(body, &wire); err != nil {
		return Board{}, fmt.Errorf("parse live board: %w", err)
	}

	board := Board{
		RowsParsed: len(wire.Data),
		HasMore:    wire.Meta.HasMore,
	}
	seenUnknown := map[string]bool{}

	for _, row := range wire.Data {
		keys, skip := singlesPlayerKeys(row)
		switch skip {
		case skipDoubles:
			board.RowsDoubles++
			continue
		case skipUnusable:
			board.RowsUnusable++
			continue
		}
		state, known := mapState(row.Status, row.EventStatus)
		if !known && !seenUnknown[row.EventStatus] {
			seenUnknown[row.EventStatus] = true
			board.UnknownEventStatuses = append(board.UnknownEventStatuses, row.EventStatus)
		}
		if state == "" {
			board.RowsUnusable++
			continue
		}
		board.Observations = append(board.Observations, Observation{
			ExternalKey:   string(row.ID),
			PlayerKeys:    keys,
			RoundCode:     row.RoundCode,
			TournamentKey: string(row.TournamentID),
			ScheduledAt:   row.ScheduledTime,
			State:         state,
			EventStatus:   row.EventStatus,
		})
	}
	return board, nil
}

// ParseFixtures разбирает ответ GET /matches?status=upcoming — форма та же,
// проекция другая: нас интересует, когда играют, а не что происходит сейчас.
func ParseFixtures(body []byte) (FixturePage, error) {
	var wire wireBoard
	if err := json.Unmarshal(body, &wire); err != nil {
		return FixturePage{}, fmt.Errorf("parse fixtures: %w", err)
	}

	page := FixturePage{
		RowsParsed: len(wire.Data),
		HasMore:    wire.Meta.HasMore,
	}
	for _, row := range wire.Data {
		keys, skip := singlesPlayerKeys(row)
		switch skip {
		case skipDoubles:
			page.RowsDoubles++
			continue
		case skipUnusable:
			page.RowsUnusable++
			continue
		}
		// Матч не состоится: окно опроса под него открывать нельзя, а в Phase 8
		// из такой строки получилась бы запись matches про игру, которой не
		// будет. В снятом срезе таких 7 из 200 — случай рядовой.
		if row.EventStatus == "Cancelled" || row.EventStatus == "Postponed" {
			page.RowsCancelled++
			continue
		}
		page.Fixtures = append(page.Fixtures, Fixture{
			ExternalKey:   string(row.ID),
			PlayerKeys:    keys,
			RoundCode:     row.RoundCode,
			TournamentKey: string(row.TournamentID),
			Tournament:    row.Tournament,
			IsQualifying:  row.IsQualifying,
			ScheduledAt:   row.ScheduledTime,
		})
	}
	return page, nil
}

// skipReason различает «отбросили, так и надо» и «не смогли прочитать».
type skipReason int

const (
	skipNone skipReason = iota
	skipDoubles
	skipUnusable
)

// singlesPlayerKeys ТРЕБУЕТ ровно двух опознаваемых одиночных игроков, а не
// пытается распознать парный разряд. Разница принципиальная: неизвестная форма
// (например p3/p4, которой мы не видели) будет пропущена, а не приведёт к
// панике по индексу. Вендор моделирует пару как одного «игрока» со своим id,
// поэтому одной проверки draw недостаточно.
func singlesPlayerKeys(row wireMatch) ([2]string, skipReason) {
	var keys [2]string
	if row.Draw == "doubles" || row.IsDoubles {
		return keys, skipDoubles
	}
	p1, p2 := row.Players.P1, row.Players.P2
	if p1.IsDoublesTeam || p2.IsDoublesTeam {
		return keys, skipDoubles
	}
	if p1.ID == "" || p2.ID == "" || p1.ID == p2.ID {
		return keys, skipUnusable
	}
	return [2]string{string(p1.ID), string(p2.ID)}, skipNone
}

// mapState переводит пару (status, event_status) в наше состояние.
//
// event_status ПЕРЕВЕШИВАЕТ status. Это не вкусовщина: в реальном срезе борта
// пришла строка со status="live" и event_status="Finished" одновременно, то
// есть протухшие live-строки существуют. Без этого правила такая строка
// поднимала бы карточку у завершённого матча.
//
// Второе возвращаемое значение — распознан ли event_status. Неизвестное
// значение НЕ угадывается: строка отбрасывается целиком.
//
// Это стоит объяснить, потому что выглядит строго. Вход в live — ОДНО
// наблюдение, поэтому новое значение вендора («abandoned», «walkover pending»,
// «suspended by weather»), пришедшее на строке, у которой status всё ещё live,
// подняло бы карточку немедленно. Падение на status — это ровно та догадка,
// которую правило запрещает. Ложный LIVE — единственный отказ, который карточка
// не переживает, а enum вендора уже разошёлся с документацией однажды
// ("Finished" в ней нет), так что случай ожидаемый, а не гипотетический.
//
// Цена честная: матч, который ИДЁТ и получил незнакомое значение, начнёт
// копить пропуски, и его карточка погаснет через ~3 цикла. Это отказ в
// безопасную сторону — карточка гаснет рано, а не загорается ложно.
// UnknownEventStatuses и TestKnownEventStatuses поднимают тревогу в обоих случаях.
func mapState(status, eventStatus string) (State, bool) {
	switch eventStatus {
	case "", "Finished", "Retired", "Walk Over", "Cancelled", "Postponed", "Interrupted":
	default:
		return "", false
	}

	// Строка про матч, которого ещё не было, — вообще не наблюдение, каким бы
	// ни был event_status. Отменённая ЗАВТРАШНЯЯ игра не должна порождать
	// «finished»: этот матч никогда не был на корте, гасить нечего.
	// Отменённая ИДУЩАЯ игра сюда доходит, потому что у неё status=live.
	if statusToState(status) == "" {
		return "", true
	}

	switch eventStatus {
	case "Finished", "Retired", "Walk Over", "Cancelled":
		return StateFinished, true
	case "Postponed":
		// матч уехал в будущее: на корте его нет, карточка должна погаснуть.
		// Чем именно он кончился, карточка не различает.
		return StateFinished, true
	case "Interrupted":
		return StateSuspended, true
	}
	return statusToState(status), true
}

func statusToState(status string) State {
	switch status {
	case "live":
		return StateOnCourt
	case "completed", "cancelled":
		return StateFinished
	}
	// upcoming и всё незнакомое — не наблюдение
	return ""
}
