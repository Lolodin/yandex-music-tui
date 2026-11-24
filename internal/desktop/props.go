package desktop

import (
	"fmt"
	"sort"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

// props — свойства объекта D-Bus (интерфейс org.freedesktop.DBus.Properties).
//
// Свой вариант вместо godbus/prop: тот при обновлении записывает новое значение в старое на месте
// (словарь Metadata дополняется, а не заменяется, ключи прошлого трека остаются), а ответ на Get
// другому клиенту в это время может кодировать то же значение в другой горутине. Для словаря это
// аварийное завершение Go («concurrent map read and map write»). Здесь каждое значение заменяется
// целиком и после публикации не изменяется, поэтому отдавать его наружу безопасно.
type props struct {
	conn *dbus.Conn
	path dbus.ObjectPath

	mu      sync.Mutex
	vals    map[string]map[string]dbus.Variant // интерфейс → свойство → значение
	setters map[string]func(dbus.Variant) *dbus.Error
	silent  map[string]bool // свойства без сигнала PropertiesChanged (Position меняется непрерывно)
}

type propDef struct {
	value  any
	set    func(dbus.Variant) *dbus.Error // nil — только чтение
	silent bool
}

func newProps(conn *dbus.Conn, path dbus.ObjectPath, defs map[string]map[string]propDef) *props {
	p := &props{
		conn:    conn,
		path:    path,
		vals:    map[string]map[string]dbus.Variant{},
		setters: map[string]func(dbus.Variant) *dbus.Error{},
		silent:  map[string]bool{},
	}
	for iface, m := range defs {
		p.vals[iface] = map[string]dbus.Variant{}
		for name, d := range m {
			p.vals[iface][name] = dbus.MakeVariant(d.value)
			if d.set != nil {
				p.setters[iface+"."+name] = d.set
			}
			if d.silent {
				p.silent[iface+"."+name] = true
			}
		}
	}
	return p
}

func errUnknownProp(iface, name string) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", []any{fmt.Sprintf("нет свойства %s.%s", iface, name)})
}

// Get — метод org.freedesktop.DBus.Properties.Get.
func (p *props) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.vals[iface][name]
	if !ok {
		return dbus.Variant{}, errUnknownProp(iface, name)
	}
	return v, nil
}

// GetAll — метод org.freedesktop.DBus.Properties.GetAll. Возвращает копию: исходный словарь
// меняется при обновлениях.
func (p *props) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.vals[iface]
	if !ok {
		return nil, dbus.NewError("org.freedesktop.DBus.Error.UnknownInterface", []any{"нет интерфейса " + iface})
	}
	out := make(map[string]dbus.Variant, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}

// Set — метод org.freedesktop.DBus.Properties.Set. Значение передаётся плееру; само свойство
// обновится, когда плеер сообщит новое состояние.
func (p *props) Set(iface, name string, v dbus.Variant) *dbus.Error {
	p.mu.Lock()
	cur, ok := p.vals[iface][name]
	set := p.setters[iface+"."+name]
	p.mu.Unlock()
	switch {
	case !ok:
		return errUnknownProp(iface, name)
	case set == nil:
		return dbus.NewError("org.freedesktop.DBus.Error.PropertyReadOnly", []any{name + " только для чтения"})
	case v.Signature() != cur.Signature():
		return dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs",
			[]any{fmt.Sprintf("%s: ожидается тип %s, передан %s", name, cur.Signature(), v.Signature())})
	}
	return set(v)
}

// update заменяет значения свойств интерфейса и рассылает PropertiesChanged по изменившимся
// (кроме «тихих»). Значения после передачи сюда не должны изменяться.
func (p *props) update(iface string, changes map[string]any) {
	changed := map[string]dbus.Variant{}
	p.mu.Lock()
	m := p.vals[iface]
	for name, val := range changes {
		v := dbus.MakeVariant(val)
		m[name] = v
		if !p.silent[iface+"."+name] {
			changed[name] = v
		}
	}
	p.mu.Unlock()
	if len(changed) > 0 {
		_ = p.conn.Emit(p.path, "org.freedesktop.DBus.Properties.PropertiesChanged", iface, changed, []string{})
	}
}

// introspection — описание свойств интерфейса для Introspect.
func (p *props) introspection(iface string) []introspect.Property {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]introspect.Property, 0, len(p.vals[iface]))
	for name, v := range p.vals[iface] {
		access := "read"
		if p.setters[iface+"."+name] != nil {
			access = "readwrite"
		}
		out = append(out, introspect.Property{Name: name, Type: v.Signature().String(), Access: access})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
