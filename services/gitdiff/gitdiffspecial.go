package gitdiff

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	"code.gitea.io/gitea/modules/base"
	"code.gitea.io/gitea/modules/charset"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	stdcharset "golang.org/x/net/html/charset"
	"golang.org/x/text/encoding"
	"golang.org/x/text/transform"
)

type SpecialDiffLine struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type SpecialDiffHunkHeader struct {
	Raw       string `json:"raw"`
	OldStart  int    `json:"old_start"`
	OldOffset int    `json:"old_offset"`
	NewStart  int    `json:"new_start"`
	NewOffset int    `json:"new_offset"`
}

type SpecialDiffHunk struct {
	Headers []string              `json:"headers"`
	Header  SpecialDiffHunkHeader `json:"header"`
	Lines   []SpecialDiffLine     `json:"lines"`
}

type SpecialDiffFile struct {
	Headers []string          `json:"headers"`
	OldPath string            `json:"old_path"`
	NewPath string            `json:"new_path"`
	Hunks   []SpecialDiffHunk `json:"hunks"`
}

func getGitDiffSpecial(gitRepo *git.Repository, opts *DiffOptions, files ...string) (*Diff, error) {
	repoPath := gitRepo.Path

	commit, err := gitRepo.GetCommit(opts.AfterCommitID)
	if err != nil {
		return nil, err
	}

	cmdDiff := git.NewCommand(gitRepo.Ctx)
	if (len(opts.BeforeCommitID) == 0 || opts.BeforeCommitID == git.EmptySHA) && commit.ParentCount() == 0 {
		cmdDiff.AddArguments("mydt").
			AddArguments(opts.WhitespaceBehavior...).
			AddArguments("4b825dc642cb6eb9a060e54bf8d69288fbee4904"). // append empty tree ref
			AddDynamicArguments(opts.AfterCommitID)
	} else {
		actualBeforeCommitID := opts.BeforeCommitID
		if len(actualBeforeCommitID) == 0 {
			parentCommit, _ := commit.Parent(0)
			actualBeforeCommitID = parentCommit.ID.String()
		}

		cmdDiff.AddArguments("mydt").
			AddArguments(opts.WhitespaceBehavior...).
			AddDynamicArguments(actualBeforeCommitID, opts.AfterCommitID)
		opts.BeforeCommitID = actualBeforeCommitID
	}

	// In git 2.31, git diff learned --skip-to which we can use to shortcut skip to file
	// so if we are using at least this version of git we don't have to tell ParsePatch to do
	// the skipping for us
	parsePatchSkipToFile := opts.SkipTo
	if opts.SkipTo != "" && git.CheckGitVersionAtLeast("2.31") == nil {
		cmdDiff.AddOptionFormat("--skip-to=%s", opts.SkipTo)
		parsePatchSkipToFile = ""
	}

	cmdDiff.AddDashesAndList(files...)

	reader, writer := io.Pipe()
	defer func() {
		_ = reader.Close()
		_ = writer.Close()
	}()

	go func() {
		stderr := &bytes.Buffer{}
		cmdDiff.SetDescription(fmt.Sprintf("GetDiffRange [repo_path: %s]", repoPath))
		if err := cmdDiff.Run(&git.RunOpts{
			Env: []string{
				"PATH=" + os.Getenv("PATH"),
				"MYDT_FORMAT=json",
			},
			Timeout: time.Duration(setting.Git.Timeout.Default) * time.Second,
			Dir:     repoPath,
			Stdout:  writer,
			Stderr:  stderr,
		}); err != nil {
			log.Error("error during GetDiff(git diff dir: %s): %v, stderr: %s", repoPath, err, stderr.String())
		}

		_ = writer.Close()
	}()

	diff, err := parseSpecialPatch(opts.MaxLines, opts.MaxLineCharacters, opts.MaxFiles, reader, parsePatchSkipToFile)
	if err != nil {
		return nil, fmt.Errorf("unable to ParsePatch: %w", err)
	}

	return diff, nil
}

func parseSpecialPatch(maxLines, maxLineCharacters, maxFiles int, reader io.Reader, skipToFile string) (*Diff, error) {
	log.Debug("parseSpecialPatch(%d, %d, %d, ..., %s)", maxLines, maxLineCharacters, maxFiles, skipToFile)

	skipping := skipToFile != ""

	diff := &Diff{Files: make([]*DiffFile, 0)}
	// sb := strings.Builder{}

	// OK let's set a reasonable buffer size.
	// This should be at least the size of maxLineCharacters or 4096 whichever is larger.
	readerSize := maxLineCharacters
	if readerSize < 4096 {
		readerSize = 4096
	}

	input := bufio.NewReaderSize(reader, readerSize)

	// parsingLoop:
	for {
		line, err := input.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return diff, nil
			}
			return diff, err
		}

		var file SpecialDiffFile
		err = json.Unmarshal(line, &file)
		if err != nil {
			return diff, err
		}

		curFile := createDiffFile(diff, file.Headers[0])

		// if maxFiles > -1 && len(diff.Files) >= maxFiles {
		// 	// 		lastFile := createDiffFile(diff, line)
		// 	// 		diff.End = lastFile.Name
		// 	// 		diff.IsIncomplete = true
		// 	// 		_, err := io.Copy(io.Discard, reader)
		// 	// 		if err != nil {
		// 	// 			// By the definition of io.Copy this never returns io.EOF
		// 	// 			return diff, fmt.Errorf("error during io.Copy: %w", err)
		// 	// 		}
		// 	break parsingLoop
		// }

		if skipping {
			if curFile.Name != skipToFile {
				// line, err = skipToNextDiffHead(input)
				// if err != nil {
				// 	if err == io.EOF {
				// 		return diff, nil
				// 	}
				// 	return diff, err
				// }
				continue
			}
			skipping = false
		}
		diff.Files = append(diff.Files, curFile)

		for _, chunk := range file.Hunks {
			curSection := &DiffSection{
				file:     curFile,
				FileName: curFile.Name,
			}
			sectionInfo := getDiffLineSectionInfo(curFile.Name, chunk.Header.Raw, 1, 1)
			curSection.Lines = append(curSection.Lines, &DiffLine{
				Type:        DiffLineSection,
				Content:     chunk.Header.Raw,
				SectionInfo: sectionInfo,
			})
			leftIdx := sectionInfo.LeftIdx
			rightIdx := sectionInfo.RightIdx
			for _, line := range chunk.Lines {
				diffLine := &DiffLine{
					Match: -1,
				}
				switch line.Type {
				case " ":
					diffLine.Type = DiffLinePlain
					diffLine.Content = " " + line.Text
					diffLine.LeftIdx = leftIdx
					diffLine.RightIdx = rightIdx
					diffLine.Match = leftIdx
					leftIdx++
					rightIdx++
				case "+":
					diffLine.Type = DiffLineAdd
					diffLine.Content = "+" + line.Text
					diffLine.RightIdx = rightIdx
					rightIdx++
					curFile.Addition++
				case "-":
					diffLine.Type = DiffLineDel
					diffLine.Content = "-" + line.Text
					diffLine.LeftIdx = leftIdx
					leftIdx++
					curFile.Deletion++
				case "m+":
					diffLine.Type = DiffLineMovedAdd
					diffLine.Content = "+" + line.Text
					diffLine.RightIdx = rightIdx
					rightIdx++
					curFile.Addition++
				case "m-":
					diffLine.Type = DiffLineMovedDel
					diffLine.Content = "-" + line.Text
					diffLine.LeftIdx = leftIdx
					leftIdx++
					curFile.Deletion++
				}
				curSection.Lines = append(curSection.Lines, diffLine)
			}
			curFile.Sections = append(curFile.Sections, curSection)
		}

		diff.TotalAddition += curFile.Addition
		diff.TotalDeletion += curFile.Deletion
	}

	// TODO: There are numerous issues with this:
	// - we might want to consider detecting encoding while parsing but...
	// - we're likely to fail to get the correct encoding here anyway as we won't have enough information
	diffLineTypeBuffers := make(map[DiffLineType]*bytes.Buffer, 3)
	diffLineTypeDecoders := make(map[DiffLineType]*encoding.Decoder, 3)
	diffLineTypeBuffers[DiffLinePlain] = new(bytes.Buffer)
	diffLineTypeBuffers[DiffLineAdd] = new(bytes.Buffer)
	diffLineTypeBuffers[DiffLineDel] = new(bytes.Buffer)
	for _, f := range diff.Files {
		f.NameHash = base.EncodeSha1(f.Name)

		for _, buffer := range diffLineTypeBuffers {
			buffer.Reset()
		}
		for _, sec := range f.Sections {
			for _, l := range sec.Lines {
				if l.Type == DiffLineSection {
					continue
				}
				diffLineTypeBuffers[l.Type].WriteString(l.Content[1:])
				diffLineTypeBuffers[l.Type].WriteString("\n")
			}
		}
		for lineType, buffer := range diffLineTypeBuffers {
			diffLineTypeDecoders[lineType] = nil
			if buffer.Len() == 0 {
				continue
			}
			charsetLabel, err := charset.DetectEncoding(buffer.Bytes())
			if charsetLabel != "UTF-8" && err == nil {
				encoding, _ := stdcharset.Lookup(charsetLabel)
				if encoding != nil {
					diffLineTypeDecoders[lineType] = encoding.NewDecoder()
				}
			}
		}
		for _, sec := range f.Sections {
			for _, l := range sec.Lines {
				decoder := diffLineTypeDecoders[l.Type]
				if decoder != nil {
					if c, _, err := transform.String(decoder, l.Content[1:]); err == nil {
						l.Content = l.Content[0:1] + c
					}
				}
			}
		}
	}

	diff.NumFiles = len(diff.Files)
	return diff, nil
}
