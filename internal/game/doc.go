// Package game implements the tossup/bonus state machine and scoring rules.
//
// State is derived: it is the fold of every non-reverted event in the log
// over a pure transition function. Undo reverts an action and everything
// after it, then replays. An invalid command is rejected without changing
// state; a rejected command is never logged.
//
// Transitions (phase checks precede ID checks: wrong phase is not_allowed,
// a mismatched tossupId/buzzId/bonusId/actionId is stale):
//
//	Idle           start_tossup                 -> Reading
//	Reading        buzz (eligible)              -> BuzzWindow
//	Reading        next                         -> Idle (tossup dead or skipped)
//	BuzzWindow     buzz (eligible)              -> BuzzWindow
//	BuzzWindow     window closes                -> AwaitingRuling (earliest judged press wins; others lost)
//	AwaitingRuling rule power / correct         -> Bonus, or Idle when the ruleset has no bonuses
//	AwaitingRuling rule neg / no_penalty        -> Reading with the buzzer locked out,
//	                                               or TossupDead if every team is locked out
//	Bonus          rule_bonus                   -> Bonus
//	Bonus          next                         -> Idle
//	TossupDead     next                         -> Idle
//	any            undo (latest action)         -> the state before that action
//
// The moderator decides powers and negs: QBuzzer can't know which word a
// player heard over voice chat. Team changes are allowed only in Idle, Bonus,
// and TossupDead. Protests may be filed in any phase for the current or an
// earlier tossup.
package game
