// Package paths centralizes the hardcoded FHS layout of rukh.
//
// Production code relies ONLY on these constants; overrides (e.g. the
// --config flag) exist solely for testing.
package paths

const (
	// Binary is where the installed executable lives.
	Binary = "/usr/sbin/rukh"

	// ConfigDir holds all configuration; never rewritten at runtime.
	ConfigDir  = "/etc/rukh"
	ConfigFile = ConfigDir + "/config.yaml"

	// HintsDir holds one optional YAML file per virtual host, named
	// after the host, listing resources to announce in Early Hints
	// when traffic cannot teach them (a CDN in front, a brand new
	// site).
	HintsDir = ConfigDir + "/hints"

	// LogDir holds all log files and runtime state. It is the only
	// path the daemon is guaranteed to be able to write to.
	LogDir = "/var/log/rukh"

	// RunDir holds the pidfile and the local status socket (tmpfs,
	// managed by systemd via RuntimeDirectory=).
	RunDir  = "/run/rukh"
	Socket  = RunDir + "/rukh.sock"
	Pidfile = RunDir + "/rukh.pid"

	// NginxConf is the conventional entry point of the nginx
	// configuration, parsed to discover virtual hosts and their
	// certificates (overridable in config.yaml).
	NginxConf = "/etc/nginx/nginx.conf"

	// Deploy targets used by --init.
	UnitFile      = "/etc/systemd/system/rukh.service"
	LogrotateFile = "/etc/logrotate.d/rukh"
)

// Log file names, to be joined with LogDir.
const (
	ServiceLog = "rukh.log"
	AccessLog  = "access.log"
	LearnLog   = "learn.log"
)
