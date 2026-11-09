package storage

import (
	"fmt"
	"sort"
	"time"
)

// Отбор игрового дня Daily picks (docs/specs/daily-picks.md в iOS-репозитории, §2 и «Rally
// decisions»). Чистая логика без БД: SQL отдаёт кандидатов, здесь решается турнир, матчи и их
// статус для игры.

const (
	picksMinMatches = 3
	picksMaxMatches = 6
	// Матч, не сыгранный к 04:00 местного времени следующего дня, аннулируется (§4.2).
	picksVoidHour = 4
)

// pickCandidate — матч, годный в игру: одиночка основной сетки, оба игрока известны, день
// известен (scheduled_at не null). Статус в отбор не входит — иначе список менялся бы, когда
// матчи начинаются.
type pickCandidate struct {
	MatchID     int64
	Edition     string
	Brand       string
	Name        string
	Round       string
	Category    *string
	ScheduledAt time.Time
	Status      string // scheduled | live | completed | cancelled
	Outcome     string // normal | retirement | walkover | default
	WinnerSide  *int
	BracketPos  *int
	A, B        MatchPlayer
	Followed    bool // в матче играет подписка пользователя
}

// civilDate — календарная дата без зоны.
type civilDate struct {
	Y int
	M time.Month
	D int
}

func (d civilDate) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Y, d.M, d.D) }

func civilOf(t time.Time) civilDate { return civilDate{t.Year(), t.Month(), t.Day()} }

// gameDayDate — местная дата турнира, на которую назначен матч.
func gameDayDate(c pickCandidate, zone *time.Location) civilDate {
	return civilOf(c.ScheduledAt.In(zone))
}

// selectGameDay — §2.1 (турнир) и §2.2 (матчи). Кандидаты — матчи одного тура на один
// игровой день. Турнир: с подпиской → выше уровень → больше матчей → имя. Матчи: сначала с
// подпиской, потом по лучшему рангу пары; берём 6, нужно минимум 3; показываем по времени.
func selectGameDay(cands []pickCandidate) (edition string, chosen []pickCandidate, ok bool) {
	type group struct {
		edition, name string
		category      *string
		followed      bool
		matches       []pickCandidate
	}
	byEdition := map[string]*group{}
	var groups []*group
	for _, c := range cands {
		g := byEdition[c.Edition]
		if g == nil {
			g = &group{edition: c.Edition, name: c.Name, category: c.Category}
			byEdition[c.Edition] = g
			groups = append(groups, g)
		}
		g.followed = g.followed || c.Followed
		g.matches = append(g.matches, c)
	}
	var eligible []*group
	for _, g := range groups {
		if len(g.matches) >= picksMinMatches {
			eligible = append(eligible, g)
		}
	}
	if len(eligible) == 0 {
		return "", nil, false
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if a.followed != b.followed {
			return a.followed
		}
		wa, wb := 0, 0
		if a.category != nil {
			wa = categoryWeight[*a.category]
		}
		if b.category != nil {
			wb = categoryWeight[*b.category]
		}
		if wa != wb {
			return wa > wb
		}
		if len(a.matches) != len(b.matches) {
			return len(a.matches) > len(b.matches)
		}
		return a.name < b.name
	})
	best := eligible[0]

	ms := append([]pickCandidate(nil), best.matches...)
	sort.SliceStable(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		if a.Followed != b.Followed {
			return a.Followed
		}
		ra, rb := bestRank(a), bestRank(b)
		if ra != rb {
			return ra < rb
		}
		return a.MatchID < b.MatchID
	})
	if len(ms) > picksMaxMatches {
		ms = ms[:picksMaxMatches]
	}
	sortByStart(ms)
	return best.edition, ms, true
}

// sortByStart — порядок показа: по времени, при равном — по id (порядку в расписании).
func sortByStart(ms []pickCandidate) {
	sort.SliceStable(ms, func(i, j int) bool {
		if !ms[i].ScheduledAt.Equal(ms[j].ScheduledAt) {
			return ms[i].ScheduledAt.Before(ms[j].ScheduledAt)
		}
		return ms[i].MatchID < ms[j].MatchID
	})
}

// bestRank — ранг лучшего игрока пары; без рангов — в конец.
func bestRank(c pickCandidate) int {
	best := 1 << 30
	for _, r := range []*int{c.A.Rank, c.B.Rank} {
		if r != nil && *r < best {
			best = *r
		}
	}
	return best
}

// pickStatus — статус матча для игры: scheduled | live | finished | void и причина void
// (walkover | cancelled | unplayed). Снятие по ходу матча (retirement, default) — обычный
// результат: победитель официальный.
func pickStatus(c pickCandidate, now time.Time, zone *time.Location) (status, voidReason string) {
	switch {
	case c.Status == "cancelled":
		return "void", "cancelled"
	case c.Status == "completed" && c.Outcome == "walkover":
		return "void", "walkover"
	case c.Status == "completed":
		return "finished", ""
	}
	d := gameDayDate(c, zone)
	cutoff := time.Date(d.Y, d.M, d.D+1, picksVoidHour, 0, 0, 0, zone)
	if !now.Before(cutoff) {
		return "void", "unplayed"
	}
	return c.Status, ""
}
