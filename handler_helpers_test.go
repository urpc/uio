package uio

// liveFuncs is a Handler, TrafficObserver and ServeHooks that reads its
// FuncHandler's fields at every call, unlike Serve's resolution of a
// FuncHandler, which takes them once. Tests that set or wrap one callback at a
// time, some after the callbacks were resolved, install it through funcs.
type liveFuncs struct{ fh *FuncHandler }

func (l *liveFuncs) OnOpen(c Conn)                     { l.fh.OnOpen(c) }
func (l *liveFuncs) OnData(c Conn) error               { return l.fh.OnData(c) }
func (l *liveFuncs) OnClose(c Conn, err error)         { l.fh.OnClose(c, err) }
func (l *liveFuncs) OnInbound(c Conn, readBytes int)   { l.fh.OnInbound(c, readBytes) }
func (l *liveFuncs) OnOutbound(c Conn, writeBytes int) { l.fh.OnOutbound(c, writeBytes) }
func (l *liveFuncs) OnStart(ev *Events)                { l.fh.OnStart(ev) }
func (l *liveFuncs) OnStop(ev *Events)                 { l.fh.OnStop(ev) }

// funcs returns the FuncHandler holding ev's callbacks in these tests. The
// first call installs it, keeping any FuncHandler withFuncs installed, as
// ev's Handler, Observer and Hooks through liveFuncs, and resolves them, so a
// test may set a callback whenever it likes, Serve or no Serve.
func funcs(ev *Events) *FuncHandler {
	if live, ok := ev.Handler.(*liveFuncs); ok {
		return live.fh
	}
	fh, ok := asFuncs(ev.Handler)
	if !ok || fh == nil {
		fh = &FuncHandler{}
	}
	live := &liveFuncs{fh: fh}
	ev.Handler, ev.Observer, ev.Hooks = live, live, live
	ev.cb = resolveCallbacks(live, live, live)
	return fh
}

// withFuncs installs fh as ev's Handler, Observer and Hooks and returns ev.
// It resolves them at once as well, for tests that drive connections without
// Serve, which is where that otherwise happens.
func withFuncs(ev *Events, fh FuncHandler) *Events {
	ev.Handler, ev.Observer, ev.Hooks = fh, fh, fh
	ev.cb = resolveCallbacks(fh, fh, fh)
	return ev
}
