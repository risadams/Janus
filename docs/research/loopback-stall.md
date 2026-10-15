# Spike: large bodies stalling on loopback in CI

Issue: #215 · Date: 2026-10-15 · Status: complete

## Question

`TestLargeBodiesRepeatedly` sends a 2 MB request and gets a 1 MB answer
800 times, directly and through Janus. On CI's Linux runner it sometimes
ran for many minutes: the release build once spent 510 s on it (ab82143),
and transfers that normally take milliseconds stalled. Is Janus delaying
or dropping data, or is it the test's environment?

## Decision

**The environment, not Janus.** Under CPU starvation on Linux, the
kernel's receive-buffer sizing on a loopback socket can collapse while a
large body arrives in pieces, and the transfer then crawls for about five
seconds. A plain Go server writing in pieces, with no Janus involved,
stalls the same way. The test now fixes the receive buffer on every socket
it controls (`testReceiveBuffer` in `internal/proxy/fidelity_test.go`), and
the stalls stopped. Janus's code is unchanged.

## Setup

- GitHub's `ubuntu-latest` runner, kernel `6.17.0-1022-azure`, Go 1.27.2.
  `tcp_rmem 4096 131072 33554432`, `tcp_moderate_rcvbuf 1`,
  `tcp_rto_min_us 200000`.
- The test looped with the `stress` workflow (#221): `-count=200 -cpu=1 -v`,
  either the whole `internal/proxy` package or the test alone. One CPU is
  what makes it show; with the default CPU count it was not seen.
- Temporary instrumentation (on #218, removed): the time of every transfer
  over 1 s and which leg it took; a goroutine dump 2 s into the first slow
  one; `ss -tinmoe` for the connection at 2 s and 4 s; and a third leg, a
  direct server that writes its answer in 32 kB pieces with a flush after
  each, as Janus relays.

## What was seen

**Slow transfers, by leg** (one CPU, 45 minutes each):

| Run | Direct | Through Janus | Direct, in flushed pieces |
| --- | --- | --- | --- |
| Whole package, buffers left to the kernel | 0 | 530 | 0 |
| The test alone, buffers left to the kernel | 0 | 354 | **86** |
| The test alone, client buffer fixed | 0 | 1 | 0 |
| The test alone, every buffer fixed (200 passes) | 0 | 0 | 0 |
| Whole package, every buffer fixed (100 passes) | 0 | 0 | 0 |

A slow transfer took 4.99 to 5.2 s, arrived whole, and once one connection
started stalling, nearly every later transfer on it did too.

**The kernel's view of a stalled response** (through Janus, 2 s in):

| | Sender | Receiver (the test client) |
| --- | --- | --- |
| Queues | 655,607 bytes not yet sent | empty: it read all it was given |
| Buffer | | **40,836 bytes**, down from the default 131,072 |
| Window | **40,960 bytes**, window-limited 90% of the time, zero-window probe timer running | |
| Damage | 1.98 MB retransmitted, congestion window 4 | **38 packets dropped**, 10 out of order |

In the next 2 s the sender moved 409,600 bytes: 200 kB/s, on a link that
otherwise moves gigabytes a second.

**The same on the other connection.** One stall, early in a run, had Janus
holding the whole request and sending nothing back. The goroutine dump
showed Janus waiting on the upstream, its transport having read the whole
request from the client, and the test's upstream server still waiting for
request bytes. That is the request crossing from Janus to the upstream, a
second loopback connection, stalling the same way. Fixing the buffer on the
test's servers as well as its client removed it.

## Why pieces matter

A transfer written in one call (the direct leg: one 1 MB write) never
stalled. One written in pieces did, whether Janus wrote them or a plain
server did. A likely reason, not shown here, is that each piece costs the
receiver more buffer per byte, so a reader that falls behind gets its
buffer and window clamped rather than grown. The kernel version and its
autotuning are part of it: none of this was seen on macOS, with 32,000
transfers at one CPU.

## What it means for Janus

- Janus relays an answer as it arrives, flushing each piece, so that
  Claude Code sees tokens as they come. That is the write pattern that
  triggers this, and it should not change.
- The stall needs a large body, a loopback connection, Linux, and a reader
  that can't keep up. Claude Code's answers are streams of small events and
  its requests leave Janus over the network, not loopback, so normal use is
  unlikely to meet it. A busy single-core Linux machine sending large
  requests to Janus might.

## Not shown

- Why this kernel clamps the buffer rather than grows it. The cost per
  piece fits the evidence but was not tested directly (kernel counters
  such as `TCPRcvPruned` were not captured).
- Whether it happens in real use between Claude Code and Janus.
- Whether other kernel versions behave the same. Only one was tested.
- Whether the request direction stalls as often as the answer direction;
  it was seen once.
