//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos)

package proxy

// echoEnabled cannot read echo settings on systems without termios, such as Windows.
func echoEnabled(int) (bool, error) {
	return false, ErrEchoUnsupported
}
