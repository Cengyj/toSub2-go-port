package protocol

import "tosub2-go-port/internal/mail"

// OTPCandidate and the legacy protocol helper names remain source compatible.
// Keep one parser for protocol clients, the console poller and SMS providers so
// timestamp filtering and duplicate-message identities cannot drift apart.
type OTPCandidate = mail.Candidate

func ExtractMailboxOTPCandidates(raw string) []OTPCandidate {
	return mail.ExtractCandidates(raw)
}

func FilterMailboxCandidatesByRequestTime(candidates []OTPCandidate, requestedAt int64) []OTPCandidate {
	return mail.FilterByRequestTime(candidates, requestedAt)
}
