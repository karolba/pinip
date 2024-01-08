package terminalscreen

import (
	"log"
	"strconv"
)

type Screen struct {
	lines                   []*Line
	desiredWidth, maxHeight int
	positionX, positionY    int

	parser EscapeSequenceParser

	Ended                  bool
	QueuedScrollbackOutput []byte

	currentSGRs SGRList
}

func NewScreen(width int, height int) *Screen {
	screen := &Screen{
		desiredWidth: width,
		maxHeight:    height,
		lines:        []*Line{NewLine()},
	}
	screen.parser = NewEscapeSequenceParser(screen)
	if screen.maxHeight <= 0 {
		screen.maxHeight = 1
	}
	if screen.desiredWidth <= 0 {
		screen.desiredWidth = 1
	}
	return screen
}

func (s *Screen) Advance(b []byte) {
	//log.Printf("call to Screen.Advance(%v)\n", b)
	s.parser.Advance(b)
}

func (s *Screen) Resize(width, height int) {
}

func (s *Screen) currentLine() *Line {
	return s.lines[s.positionY]
}

func (s *Screen) currentScreenHeight() int {
	return len(s.lines)
}

func (s *Screen) sendLineToScrollbackBuffer(line *Line) {
	didSetSGR := false

	// Prepend every non-first line with a line terminator to make the scrollback buffer not end with a newline.
	if len(s.QueuedScrollbackOutput) > 0 {
		s.appendToScrollback("\n")
	}

	previousCharacter := Character{}
	for _, character := range line.characters {
		s.appendToScrollback(character.extraEscapeSequences)

		if !character.sgr.equals(previousCharacter.sgr) {
			// Reset SGR
			s.appendToScrollback("\033[0m")

			// Set SGR again
			for _, sgr := range character.sgr {
				s.appendToScrollback(sgr.toCSI())
			}

			didSetSGR = true
		}

		s.appendToScrollback(string(character.rune))

		previousCharacter = character
	}

	// TODO: don't reset the last line, as the child might expect it to be set
	//       do something similar to how '\n' is handled
	if didSetSGR {
		// Reset SGR
		s.appendToScrollback("\033[0m")
	}
}

func (s *Screen) End() {
	if s.Ended {
		log.Panicln("Screen.End() called twice")
	}

	s.Ended = true

	for _, line := range s.lines {
		s.sendLineToScrollbackBuffer(line)
	}

	moveRightBy := s.currentLine().lengthWithoutTrailingSpacesAndEmptyRunes() - s.positionX
	if moveRightBy > 0 {
		s.appendToScrollback("\033[" + strconv.Itoa(moveRightBy) + "C")
	} else if moveRightBy < 0 {
		s.appendToScrollback("\033[" + strconv.Itoa(-moveRightBy) + "D")
	}

	moveDownBy := s.currentScreenHeight() - s.positionY - 1
	if moveDownBy > 0 {
		s.appendToScrollback("\033[" + strconv.Itoa(moveDownBy) + "B")
	} else if moveDownBy < 0 {
		s.appendToScrollback("\033[" + strconv.Itoa(-moveDownBy) + "A")
	}

	s.lines = []*Line{}
}

func (s *Screen) nextLine() {
	//log.Printf("call to Screen.nextLine()\n")

	if s.positionY < s.currentScreenHeight()-1 {
		s.positionY++
	} else {
		s.lines = append(s.lines, NewLine())
	}

	// If there's more than s.maxHeight lines, send the first line to the scrollback buffer and remove it
	// from the virtual screen.
	if s.currentScreenHeight() > s.maxHeight {
		s.sendLineToScrollbackBuffer(s.lines[0])
		s.lines = append([]*Line{}, s.lines[1:]...)
	}
}

func (s *Screen) prevLine() {
	if s.positionY <= 0 {
		// TODO: negative-index lines?
		return
	}
	s.positionY--
}

func (s *Screen) nextCharacter() {
	// Don't care about max line length - we pretend the screen is infinitely wide.
	s.positionX += 1
}

func (s *Screen) prevCharacter() {
	s.positionX -= 1
	if s.positionX < 0 {
		s.positionX = 0
	}
}

func (s *Screen) setCurrentCharacterTo(r rune) {
	currentCharacter := s.currentLine().characterAt(s.positionX)

	currentCharacter.rune = r
	if s.currentSGRs == nil {
		currentCharacter.clearSGR()
	} else {
		for _, sgr := range s.currentSGRs {
			currentCharacter.addSGR(sgr)
		}
	}
}

func (s *Screen) outNormalCharacter(b rune) {
	s.setCurrentCharacterTo(b)
	s.nextCharacter()
}

func (s *Screen) outRelativeMoveCursorVertical(howMany int) {
	// TODO: maybe this shouldn't iterate here
	for i := 0; howMany > i; i++ {
		s.nextLine()
	}
	for i := 0; howMany < i; i-- {
		s.prevLine()
	}
}

func (s *Screen) outRelativeMoveCursorHorizontal(howMany int) {
	s.positionX += howMany
	if s.positionX < 0 {
		s.positionX = 0
	}
}

func (s *Screen) outAbsoluteMoveCursorVertical(targetY int) {
	moveDownBy := targetY - s.positionY
	s.outRelativeMoveCursorVertical(moveDownBy)
}

func (s *Screen) outAbsoluteMoveCursorHorizontal(targetX int) {
	s.positionX = targetX
	if s.positionX < 0 {
		s.positionX = 0
	}
}

func (s *Screen) outDeleteLeft(howMany int) {
	for i := 0; howMany > i; i++ {
		s.prevCharacter()
		s.setCurrentCharacterTo(' ')
		if s.positionX == 0 {
			break
		}
	}
}

func (s *Screen) outUnhandledEscapeSequence(seq string) {
	// append to the current character but don't move the cursor forward
	s.currentLine().characterAt(s.positionX).extraEscapeSequences += seq
}

func (s *Screen) outSelectGraphicRenditionAttribute(sgr SelectGraphicRenditionAttribute) {
	if sgr.isUnsetAll() {
		s.currentSGRs = []SelectGraphicRenditionAttribute{}
	} else {
		sgr.addToSGRAttributeList(&s.currentSGRs)
	}
}

func (s *Screen) appendToScrollback(str string) {
	s.QueuedScrollbackOutput = append(s.QueuedScrollbackOutput, []byte(str)...)
}