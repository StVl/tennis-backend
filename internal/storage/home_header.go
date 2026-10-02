package storage

import (
	"sort"
	"time"
)

// Статус в заголовке главной: «Tuesday, Carlos plays at 09:00.» Сервер выбирает, КАКОЙ статус
// (спецификация главной, §4.1, первое совпадение выигрывает), клиент собирает копию: EN/RU,
// 12/24 часа и нейтральные формулировки при скрытых счетах (§10.4) — поэтому наружу уходят
// вид и факты, а не готовая строка.

// Если подходящих подписок несколько, называется та, у которой выше рейтинг ATP (без рейтинга —
// последними, при равенстве — порядок подписок): так решил продукт поверх §4.1.

// HeaderStatus — один из видов §4.1.
type HeaderStatus struct {
	// title_day | plays_today | on_court | semis_day | quarters_day | champion_yesterday |
	// lost_yesterday | tournament_day | final_day | season
	Kind   string        `json:"kind"`
	Player *HeaderPlayer `json:"player,omitempty"`
	// Время матча для plays_today.
	At *time.Time `json:"at,omitempty"`
	// Сколько подписок на корте сегодня (on_court). У on_court есть и player — подписка с самым
	// высоким рейтингом: «Carlos and 2 more on court.»
	Count int `json:"count,omitempty"`
	// Город турнира: tournament_day, final_day, а у title_day — для нейтральной копии
	// «final day in Chengdu» при скрытых счетах.
	City string `json:"city,omitempty"`
	Day  int    `json:"day,omitempty"`
}

type HeaderPlayer struct {
	Slug      string `json:"slug"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// headerMatch — матч подписки сегодня или вчера (по местному времени пользователя).
type headerMatch struct {
	Player HeaderPlayer
	Round  string
	At     *time.Time
	// completed / live / scheduled
	Status string
	Won    bool
	// Город турнира — для title_day.
	City string
	// Рейтинг ATP игрока: 0 — нет. По нему выбирается, кого назвать.
	Rank int
	// Порядок подписки — при равном рейтинге.
	Order int
}

// byRank — сначала высокий рейтинг, игроки без рейтинга в конце, при равенстве порядок подписок.
func byRank(ms []headerMatch) []headerMatch {
	out := append([]headerMatch(nil), ms...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Rank == 0) != (b.Rank == 0) {
			return b.Rank == 0
		}
		if a.Rank != b.Rank {
			return a.Rank < b.Rank
		}
		return a.Order < b.Order
	})
	return out
}

// headerTournament — идущий турнир в порядке карусели.
type headerTournament struct {
	City          string
	Day           int
	FinalToday    bool
	SemisToday    bool
	QuartersToday bool
}

type headerFacts struct {
	Today     []headerMatch
	Yesterday []headerMatch
	Live      []headerTournament
}

// pickHeaderStatus — §4.1, первое совпадение выигрывает.
func pickHeaderStatus(f headerFacts) HeaderStatus {
	f.Today, f.Yesterday = byRank(f.Today), byRank(f.Yesterday)
	// 1. Подписка играет финал сегодня.
	for _, m := range f.Today {
		if m.Round == "F" {
			p := m.Player
			return HeaderStatus{Kind: "title_day", Player: &p, City: m.City}
		}
	}
	// 2–3. Сколько подписок на корте сегодня.
	players := map[string]headerMatch{}
	for _, m := range f.Today {
		if cur, ok := players[m.Player.Slug]; !ok || earlier(m.At, cur.At) {
			players[m.Player.Slug] = m
		}
	}
	if len(players) == 1 {
		for _, m := range players {
			p := m.Player
			return HeaderStatus{Kind: "plays_today", Player: &p, At: m.At}
		}
	}
	if len(players) >= 2 {
		top := f.Today[0].Player // f.Today уже отсортирован по рейтингу
		return HeaderStatus{Kind: "on_court", Count: len(players), Player: &top}
	}
	// 4–5. День полуфиналов / четвертьфиналов в идущем турнире.
	for _, t := range f.Live {
		if t.SemisToday {
			return HeaderStatus{Kind: "semis_day", City: t.City}
		}
	}
	for _, t := range f.Live {
		if t.QuartersToday {
			return HeaderStatus{Kind: "quarters_day", City: t.City}
		}
	}
	// 6. Вчера подписка взяла титул.
	for _, m := range f.Yesterday {
		if m.Round == "F" && m.Status == "completed" && m.Won {
			p := m.Player
			return HeaderStatus{Kind: "champion_yesterday", Player: &p, City: m.City}
		}
	}
	// 7. Вчера подписка проиграла.
	for _, m := range f.Yesterday {
		if m.Status == "completed" && !m.Won {
			p := m.Player
			return HeaderStatus{Kind: "lost_yesterday", Player: &p, City: m.City}
		}
	}
	// 8–9. Обычный день турнира; финальный — «final in Chengdu».
	if len(f.Live) > 0 {
		t := f.Live[0]
		if t.FinalToday {
			return HeaderStatus{Kind: "final_day", City: t.City}
		}
		return HeaderStatus{Kind: "tournament_day", City: t.City, Day: t.Day}
	}
	// 10. Идущих турниров нет.
	return HeaderStatus{Kind: "season"}
}

func earlier(a, b *time.Time) bool {
	switch {
	case a == nil:
		return false
	case b == nil:
		return true
	}
	return a.Before(*b)
}
