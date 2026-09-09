package auditfeat

// Reader can read a string.
type Reader interface {
	Read() (string, error)
}

// Writer can write a string.
type Writer interface {
	Write(string) error
}

// ReadWriter can both read and write.
type ReadWriter interface {
	Reader
	Writer
}

// baseServer holds a server's name.
type baseServer struct {
	name string
}

// Name returns b's name.
func (b *baseServer) Name() string { return b.name }

// Server is a named network server, embedding baseServer for its
// promoted Name method.
type Server struct {
	baseServer
	Addr string
}

// Config configures a Server.
type Config struct {
	Name string `json:"name" yaml:"name"`
	Port int    `json:"port,omitempty"`
}
