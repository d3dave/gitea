package gitdiff

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
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

type DifftChange struct {
	Start   uint32 `json:"start"`
	End     uint32 `json:"end"`
	Content string `json:"content"`
}

type DifftSide struct {
	LineNumber uint32        `json:"line_number"`
	Changes    []DifftChange `json:"changes"`
}

type DifftLine struct {
	Lhs DifftSide `json:"lhs"`
	Rhs DifftSide `json:"rhs"`
}

type DifftHunk []DifftLine

type DifftFile struct {
	Path     string      `json:"path"`
	Language string      `json:"language"`
	Status   string      `json:"status"`
	Chunks   []DifftHunk `json:"chunks"`
}

func getDifft(gitRepo *git.Repository, opts *DiffOptions, files ...string) (*Diff, error) {
	repoPath := gitRepo.Path

	commit, err := gitRepo.GetCommit(opts.AfterCommitID)
	if err != nil {
		return nil, err
	}

	cmdDiff := git.NewCommand(gitRepo.Ctx)
	if (len(opts.BeforeCommitID) == 0 || opts.BeforeCommitID == git.EmptySHA) && commit.ParentCount() == 0 {
		cmdDiff.AddArguments("diff", "--src-prefix=\\a/", "--dst-prefix=\\b/", "-M").
			AddArguments(opts.WhitespaceBehavior...).
			AddArguments("4b825dc642cb6eb9a060e54bf8d69288fbee4904"). // append empty tree ref
			AddDynamicArguments(opts.AfterCommitID)
	} else {
		actualBeforeCommitID := opts.BeforeCommitID
		if len(actualBeforeCommitID) == 0 {
			parentCommit, _ := commit.Parent(0)
			actualBeforeCommitID = parentCommit.ID.String()
		}

		cmdDiff.AddArguments("diff", "--src-prefix=\\a/", "--dst-prefix=\\b/", "-M").
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
				"DFT_UNSTABLE=yes",
				"DFT_DISPLAY=json",
				"GIT_EXTERNAL_DIFF=difft",
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

	diff, err := parseDifftPatch(opts.MaxLines, opts.MaxLineCharacters, opts.MaxFiles, reader, parsePatchSkipToFile)
	if err != nil {
		return nil, fmt.Errorf("unable to parseDifftPatch: %w", err)
	}

	return diff, nil
}

// ParsePatch builds a Diff object from a io.Reader and some parameters.
func parseDifftPatch(maxLines, maxLineCharacters, maxFiles int, reader io.Reader, skipToFile string) (*Diff, error) {
	log.Debug("parseDifftPatch(%d, %d, %d, ..., %s)", maxLines, maxLineCharacters, maxFiles, skipToFile)

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

		var file DifftFile
		err = json.Unmarshal(line, &file)
		if err != nil {
			return diff, err
		}

		curFile := &DiffFile{
			Name:     file.Path,
			Index:    len(diff.Files) + 1,
			Type:     DiffFileChange,
			Sections: make([]*DiffSection, 0, 10),
		}

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

		for _, chunk := range file.Chunks {
			curSection := &DiffSection{
				file:     curFile,
				FileName: curFile.Name,
			}
			curSection.Lines = append(curSection.Lines, &DiffLine{
				Type:        DiffLineSection,
				Content:     "@",
				SectionInfo: nil,
			})
			for _, line := range chunk {
				for _, dc := range line.Lhs.Changes {
					diffLine := &DiffLine{
						Type:        DiffLineDel,
						Content:     "-" + dc.Content,
						SectionInfo: nil,
					}
					curSection.Lines = append(curSection.Lines, diffLine)
					curFile.Deletion++
				}
				for _, dc := range line.Rhs.Changes {
					diffLine := &DiffLine{
						Type:        DiffLineAdd,
						Content:     "+" + dc.Content,
						SectionInfo: nil,
					}
					curSection.Lines = append(curSection.Lines, diffLine)
					curFile.Addition++
				}
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
