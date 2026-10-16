package engine

import (
	"testing"

	"perco-meal/internal/config"
	"perco-meal/internal/perco"
	"perco-meal/internal/store"
)

func TestClassify(t *testing.T) {
	cfg := &config.Config{DeviceIDs: []int{2}, PassEventIDs: []int{17}, DoorEventIDs: []int{31}}
	e := &Engine{cfg: cfg}
	dev2, dev3 := 2, 3
	cases := []struct {
		code int
		name string
		dev  *int
		want string
	}{
		{17, "Проход по идентификатору", &dev2, store.KindPass},
		{529, "Проход по идентификатору: с несоответствием временным критериям доступа", &dev2, store.KindDeny},
		{273, "Проход по идентификатору: с несоответствием текущему местоположению", &dev2, store.KindDeny},
		{31, "ИУ не закрыто после прохода по идентификатору: ", &dev2, store.KindDoor},
		{1, "Идентификатор не зарегистрирован", &dev2, store.KindDeny},
		{17, "Проход по идентификатору", &dev3, ""},                   // other door, not the canteen
		{500000, "Вход оператора в систему", nil, ""},                 // operator events are ignored
		{9999, "Запрет прохода: что-то новое", &dev2, store.KindDeny}, // unknown refusal caught by name
	}
	for _, c := range cases {
		got := e.classify(perco.Event{EventNameID: c.code, EventName: c.name, DeviceID: c.dev})
		if got != c.want {
			t.Errorf("classify(%d %q) = %q, want %q", c.code, c.name, got, c.want)
		}
	}
}
