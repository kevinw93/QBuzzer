// Package clock estimates each client's clock offset and network round-trip
// time from NTP-style four-timestamp exchanges.
//
// All times are float64 milliseconds. Server times are on a Server clock
// (monotonic, since process start); client times are on the client's
// performance clock (performance.timeOrigin + performance.now()).
//
// For one exchange (server send T1, client receive T2, client send T3, server
// receive T4):
//
//	RTT    = (T4 - T1) - (T3 - T2)
//	Offset = ((T2 - T1) + (T3 - T4)) / 2   // client minus server
//
// If the client reports honestly and neither clock drifts during the
// exchange, the true offset is within Offset ± RTT/2 for any split of the
// delay between the two directions. Client-reported T2 and T3 are not
// trusted for anything beyond that: a lying client can move its own offset,
// which adjudication must cap.
package clock
