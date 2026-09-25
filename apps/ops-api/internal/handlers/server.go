package handlers

// Server bundles deps for handler methods. Aliased to Deps so each handler can
// be a method on *Server without re-passing every dep through context.
type Server struct{ *Deps }

func NewServer(d *Deps) *Server { return &Server{Deps: d} }
