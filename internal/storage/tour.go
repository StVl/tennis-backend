package storage

// Tour — тур (лига): ATP или WTA. Тур игрока выводится из players.gender функцией
// tour_code_for(gender_t) в схеме (tennis-data-storage) — это единственное место соответствия
// «пол → тур». В SQL рейтинг игрока джойнится как r.tour_code = tour_code_for(p.gender),
// в JSON тур отдаётся полем "tour", чтобы клиент подписывал ранг «ATP #3» / «WTA #3».
type Tour string

const (
	TourATP Tour = "atp"
	TourWTA Tour = "wta"
)

// ParseTour — значение параметра ?tour=. Пустое — ATP: клиенты до WTA параметр не шлют и
// должны видеть ровно то же, что раньше. Регистр не прощается: неизвестное значение — ошибка
// (400 bad_tour), а не молчаливый ATP.
func ParseTour(s string) (Tour, bool) {
	switch s {
	case "", string(TourATP):
		return TourATP, true
	case string(TourWTA):
		return TourWTA, true
	}
	return "", false
}

// Gender — значение players.gender / tournament_editions.gender для тура.
func (t Tour) Gender() string {
	if t == TourWTA {
		return "f"
	}
	return "m"
}
