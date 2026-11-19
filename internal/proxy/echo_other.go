//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos)

package proxy

// echoEnabled は termios の無い OS（Windows など）では読めない。
func echoEnabled(int) (bool, error) {
	return false, ErrEchoUnsupported
}
