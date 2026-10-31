package credentialprovider_test

import "syscall"

// Killing a Windows job may reset its sockets instead of sending a FIN.
const errConnectionReset = syscall.WSAECONNRESET
