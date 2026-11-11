# Patched copy of go-libvirt

Based on `github.com/digitalocean/go-libvirt v0.0.0-20261013170128-efbbdf578ac8`
(the version pinned before this directory existed), wired in with a `replace`
directive in the root `go.mod`. No newer upstream version was available that
fixes this.

## What changed

Only `rpc.go`, `requestStream`, the deferred deregister, plus a new test file
`stream_wedge_test.go`. Everything else is byte-identical to the base version.

```go
defer func() {
    go func() { for range c {} }()   // new
    l.cmux.Lock()
    ...
    l.deregister(serial)             // closes c, which ends the drainer
}()
```

## Why

`Libvirt.callback` (rpc.go, called by the socket listener goroutine for every
incoming packet) takes `cmux.Lock()` and then does an unbuffered `c <- res`
while still holding it. A request's deferred cleanup also needs `cmux.Lock()`
to deregister. If a call stops receiving before the stream is over (the
`io.Writer` passed to `DomainOpenConsoleBidirectional`, `DomainScreenshot`,
`StorageVolDownload` returned an error because the client went away, so
`processIncomingStream` returned early), and libvirtd sends one more packet in
that window, the listener blocks on the channel holding `cmux` while the call
blocks on `cmux`. The listener is also the only reader of the socket, so every
call on that connection (all of them need `register`, which takes `cmux`) hangs
for good.

The fix drains the channel while taking the lock, so the listener can always
finish its send and release `cmux`; `deregister` then closes the channel and
the drainer exits (it also exits if `deregisterAll` already closed it).
Late packets for a deregistered serial are dropped by `callback` as before.

`TestStreamConsumerFailureDoesNotWedgeConnection` fails against the base
version (deterministically) and passes with the patch.

## Not fixed here

`DomainScreenshot` over RPC returns an XDR decode error on every call, because
`requestStream` returns the last stream packet (empty payload) instead of the
initial reply that carries the mime type. The image data is delivered to the
writer; only the return value is wrong. Left alone on purpose.
