package storage

import "time"

// Часовой пояс турнира для «игрового дня» Daily picks: день — это местная дата турнира, а не
// пользователя. В схеме у турнира есть только country_code, поэтому зона берётся из таблицы
// стран; у стран с несколькими зонами — по бренду. Неизвестное — UTC (день может съехать на
// несколько часов, но игра всё равно соберётся).

var zoneByBrand = map[string]string{
	"australian_open": "Australia/Melbourne", "adelaide": "Australia/Adelaide",
	"brisbane": "Australia/Brisbane", "hobart": "Australia/Hobart",
	"us_open": "America/New_York", "miami": "America/New_York", "cincinnati": "America/New_York",
	"washington": "America/New_York", "winston_salem": "America/New_York", "newport": "America/New_York",
	"indian_wells": "America/Los_Angeles", "san_diego": "America/Los_Angeles",
	"atlanta": "America/New_York", "dallas": "America/Chicago", "houston": "America/Chicago",
	"delray_beach": "America/New_York", "charleston": "America/New_York", "austin": "America/Chicago",
	"canada": "America/Toronto", "montreal": "America/Toronto", "toronto": "America/Toronto",
	"los_cabos": "America/Mazatlan", "acapulco": "America/Mexico_City", "monterrey": "America/Monterrey",
	"guadalajara": "America/Mexico_City", "merida": "America/Merida",
	"rio_de_janeiro": "America/Sao_Paulo", "sao_paulo": "America/Sao_Paulo",
	"wta_finals": "America/Los_Angeles", // 2026 — Indian Wells
}

var zoneByCountry = map[string]string{
	"AE": "Asia/Dubai", "AR": "America/Argentina/Buenos_Aires", "AT": "Europe/Vienna",
	"AU": "Australia/Sydney", "BE": "Europe/Brussels", "BR": "America/Sao_Paulo",
	"CA": "America/Toronto", "CH": "Europe/Zurich", "CL": "America/Santiago",
	"CN": "Asia/Shanghai", "CZ": "Europe/Prague", "DE": "Europe/Berlin", "DK": "Europe/Copenhagen",
	"ES": "Europe/Madrid", "FI": "Europe/Helsinki", "FR": "Europe/Paris", "GB": "Europe/London",
	"GR": "Europe/Athens", "HK": "Asia/Hong_Kong", "HR": "Europe/Zagreb", "HU": "Europe/Budapest",
	"IN": "Asia/Kolkata", "IT": "Europe/Rome", "JP": "Asia/Tokyo", "KR": "Asia/Seoul",
	"KZ": "Asia/Almaty", "MA": "Africa/Casablanca", "MC": "Europe/Monaco", "MX": "America/Mexico_City",
	"NL": "Europe/Amsterdam", "NO": "Europe/Oslo", "NZ": "Pacific/Auckland", "PL": "Europe/Warsaw",
	"PT": "Europe/Lisbon", "QA": "Asia/Qatar", "RO": "Europe/Bucharest", "RS": "Europe/Belgrade",
	"SA": "Asia/Riyadh", "SE": "Europe/Stockholm", "SG": "Asia/Singapore", "SI": "Europe/Ljubljana",
	"SK": "Europe/Bratislava", "TH": "Asia/Bangkok", "TR": "Europe/Istanbul", "TW": "Asia/Taipei",
	"US": "America/New_York",
}

// tournamentZone — зона турнира: сначала бренд, потом страна, иначе UTC.
func tournamentZone(brand, country string) *time.Location {
	name, ok := zoneByBrand[brand]
	if !ok {
		name, ok = zoneByCountry[country]
	}
	if !ok {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}
