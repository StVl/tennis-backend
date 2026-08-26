package livesource

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("не прочитать фикстуру %s: %v", name, err)
	}
	return b
}

// Правило 1, закреплённое типами: ни одно поле Observation не должно даже
// НАЗЫВАТЬСЯ похоже на счёт. Тест переживёт нас обоих и остановит будущий PR
// «ну одно маленькое поле».
func TestObservationHasNoScoreFields(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)score|set|game|point|serve|prob|tiebreak`)
	for _, typ := range []reflect.Type{
		reflect.TypeOf(Observation{}),
		reflect.TypeOf(Fixture{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			if name := typ.Field(i).Name; forbidden.MatchString(name) {
				t.Errorf("%s.%s: поле похоже на счёт; правило 1 запрещает счёту покидать разбор",
					typ.Name(), name)
			}
		}
	}
}

// Разбор обязан ВЫБРАСЫВАТЬ счёт, а не просто не показывать его.
// Фикстура намеренно содержит sets/games/points/server.
func TestParseDropsScore(t *testing.T) {
	raw := readFixture(t, "live_board_synthetic.json")
	if !strings.Contains(string(raw), `"points"`) {
		t.Fatal("фикстура должна содержать счёт, иначе тест ничего не проверяет")
	}
	board, err := ParseBoard(raw)
	if err != nil {
		t.Fatalf("ParseBoard: %v", err)
	}
	// сериализуем результат целиком и ищем следы счёта
	out, err := json.Marshal(board)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"points", "games", "server", "tiebreak", "sequence"} {
		if strings.Contains(strings.ToLower(string(out)), needle) {
			t.Errorf("в разобранном борте найдено %q — счёт не должен покидать парсер", needle)
		}
	}
}

// event_status перевешивает status. В реальном срезе борта пришла строка
// со status=live и event_status=Finished одновременно.
func TestMapState(t *testing.T) {
	cases := []struct {
		status, eventStatus string
		want                State
		wantKnown           bool
	}{
		{"live", "", StateOnCourt, true},
		{"live", "Finished", StateFinished, true},
		{"live", "Retired", StateFinished, true},
		{"live", "Walk Over", StateFinished, true},
		{"live", "Cancelled", StateFinished, true},
		{"live", "Postponed", StateFinished, true},
		{"live", "Interrupted", StateSuspended, true},
		{"completed", "", StateFinished, true},
		{"upcoming", "", "", true},
		// неизвестное значение: падаем на status и сообщаем, что не распознали
		{"live", "Zombie", StateOnCourt, false},
		{"upcoming", "Zombie", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.status+"/"+tc.eventStatus, func(t *testing.T) {
			got, known := mapState(tc.status, tc.eventStatus)
			if got != tc.want || known != tc.wantKnown {
				t.Fatalf("mapState(%q,%q) = (%q,%v), ожидалось (%q,%v)",
					tc.status, tc.eventStatus, got, known, tc.want, tc.wantKnown)
			}
		})
	}
}

func TestParseSyntheticBoard(t *testing.T) {
	board, err := ParseBoard(readFixture(t, "live_board_synthetic.json"))
	if err != nil {
		t.Fatalf("ParseBoard: %v", err)
	}
	if board.RowsParsed != 8 {
		t.Fatalf("RowsParsed = %d, ожидалось 8", board.RowsParsed)
	}
	if !board.HasMore {
		t.Error("HasMore потерян — а каждая следующая страница это ещё один запрос из квоты")
	}

	byKey := map[string]Observation{}
	for _, o := range board.Observations {
		byKey[o.ExternalKey] = o
	}

	want := map[string]State{
		"900001": StateOnCourt,   // обычная живая строка
		"900002": StateFinished,  // протухшая live-строка
		"900003": StateOnCourt,   // неизвестный event_status -> падаем на status
		"900004": StateSuspended, // дождь
		"900005": StateFinished,  // перенесён: на корте его нет
		"900008": StateOnCourt,   // без времени начала
	}
	for key, wantState := range want {
		o, ok := byKey[key]
		if !ok {
			t.Errorf("матч %s отсутствует в разборе", key)
			continue
		}
		if o.State != wantState {
			t.Errorf("матч %s: состояние %q, ожидалось %q", key, o.State, wantState)
		}
	}

	// парный разряд и строка без id второго игрока — отброшены
	for _, key := range []string{"900006", "900007"} {
		if _, ok := byKey[key]; ok {
			t.Errorf("матч %s не должен был попасть в разбор", key)
		}
	}
	if board.RowsSkipped != 2 {
		t.Errorf("RowsSkipped = %d, ожидалось 2", board.RowsSkipped)
	}

	if len(board.UnknownEventStatuses) != 1 || board.UnknownEventStatuses[0] != "Zombie" {
		t.Errorf("UnknownEventStatuses = %v, ожидалось [Zombie]", board.UnknownEventStatuses)
	}

	if o := byKey["900008"]; o.ScheduledAt != nil {
		t.Error("scheduled_time=null должен остаться nil: это реальное состояние, а не пробел")
	}
	if o := byKey["900001"]; o.PlayerKeys != [2]string{"34", "13"} {
		t.Errorf("PlayerKeys = %v, ожидалось [34 13]", o.PlayerKeys)
	}
	if o := byKey["900001"]; o.TournamentKey != "1221" {
		t.Errorf("TournamentKey = %q — id турнира приходит строкой, id матча числом", o.TournamentKey)
	}
}

// Настоящий борт вендора: разбирается без ошибок, парные отброшены.
func TestParseRealBoard(t *testing.T) {
	board, err := ParseBoard(readFixture(t, "live_board.json"))
	if err != nil {
		t.Fatalf("ParseBoard: %v", err)
	}
	if board.RowsParsed != 19 {
		t.Fatalf("RowsParsed = %d, ожидалось 19", board.RowsParsed)
	}
	if len(board.Observations)+board.RowsSkipped != board.RowsParsed {
		t.Errorf("строки потерялись: %d + %d != %d",
			len(board.Observations), board.RowsSkipped, board.RowsParsed)
	}
	if board.RowsSkipped == 0 {
		t.Error("в реальном срезе есть парные разряды, они должны быть отброшены")
	}
	for _, o := range board.Observations {
		if o.PlayerKeys[0] == "" || o.PlayerKeys[1] == "" {
			t.Errorf("матч %s прошёл с пустым id игрока", o.ExternalKey)
		}
	}
}

func TestParseRealFixtures(t *testing.T) {
	page, err := ParseFixtures(readFixture(t, "upcoming_board.json"))
	if err != nil {
		t.Fatalf("ParseFixtures: %v", err)
	}
	if page.RowsParsed != 200 {
		t.Fatalf("RowsParsed = %d, ожидалось 200", page.RowsParsed)
	}
	if !page.HasMore {
		t.Error("HasMore потерян: в срезе 200 из 309 строк, вторая страница обязательна")
	}
	withKey := 0
	for _, f := range page.Fixtures {
		if f.TournamentKey != "" {
			withKey++
		}
	}
	// на борте upcoming tournament_id есть везде — на этом стоит писатель матчей
	if withKey != len(page.Fixtures) {
		t.Errorf("tournament_id заполнен у %d из %d фикстур; писатель матчей на него опирается",
			withKey, len(page.Fixtures))
	}
}

// Тело, которое не разбирается — ОШИБКА, а не «ноль матчей». Иначе страница
// 502 от прокси погасит карточки у всех пользователей.
func TestParseRejectsNonJSON(t *testing.T) {
	cases := map[string][]byte{
		"html от прокси":  readFixture(t, "error_502.html"),
		"обрезанное тело": readFixture(t, "live_board.json")[:200],
		"пустое тело":     {},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			board, err := ParseBoard(body)
			if err == nil {
				t.Fatalf("ошибки нет, разобрано %d строк — это должно быть ошибкой",
					board.RowsParsed)
			}
		})
	}
}

// Пустой борт — НЕ ошибка: решение принимает вызывающий, у него есть расписание.
func TestParseEmptyBoardIsNotAnError(t *testing.T) {
	board, err := ParseBoard([]byte(`{"data":[],"meta":{"has_more":false}}`))
	if err != nil {
		t.Fatalf("пустой борт не должен быть ошибкой разбора: %v", err)
	}
	if board.RowsParsed != 0 || len(board.Observations) != 0 {
		t.Fatalf("ожидался пустой разбор, получено %+v", board)
	}
}

// Падает в тот день, когда вендор пришлёт новое значение event_status.
// Это и есть свойство «отказ превращается в красный тест, а не в инцидент».
func TestKnownEventStatuses(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		var wire wireBoard
		if err := json.Unmarshal(readFixture(t, filepath.Base(f)), &wire); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, row := range wire.Data {
			if _, known := mapState(row.Status, row.EventStatus); !known {
				// синтетическая фикстура содержит «Zombie» намеренно
				if filepath.Base(f) == "live_board_synthetic.json" {
					continue
				}
				t.Errorf("%s: нераспознанный event_status %q — спека вендора разошлась, "+
					"обнови mapState", filepath.Base(f), row.EventStatus)
			}
		}
	}
}
