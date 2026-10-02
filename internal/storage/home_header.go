package storage

import "time"

// Статус в заголовке главной: «Tuesday, Carlos plays at 09:00.» Сервер выбирает, КАКОЙ статус
// (спецификация главной, §4.1, первое совпадение выигрывает), клиент собирает копию: EN/RU,
// 12/24 часа и нейтральные формулировки при скрытых счетах (§10.4) — поэтому наружу уходят
// вид и факты, а не готовая строка.

// HeaderStatus — один из видов §4.1.
type HeaderStatus struct {
	// title_day | plays_today | on_court | semis_day | quarters_day | champion_yesterday |
	// lost_yesterday | tournament_day | final_day | season
	Kind   string        `json:"kind"`
	Player *HeaderPlayer `json:"player,omitempty"`
	// Время матча для plays_today.
	At *time.Time `json:"at,omitempty"`
	// Сколько подписок на корте сегодня (on_court).
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
		return HeaderStatus{Kind: "on_court", Count: len(players)}
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
