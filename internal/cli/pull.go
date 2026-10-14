package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/tara-vision/taraengine/pkg/api"
	"github.com/tara-vision/taraengine/pkg/client"
)

// hfRefused matches the report of a pull that Hugging Face turned down for lack of a token or of access. The
// pull events carry nothing but the message, so the status has to be read from it.
var hfRefused = regexp.MustCompile(`Hugging Face answered (401|403)\b`)

func newPullCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "pull <model | hf://owner/repo/file.gguf | /absolute/path.gguf>",
		Short: "Pull a catalog model with its siblings, or register another GGUF",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := flags.connect(cmd)
			if err != nil {
				return err
			}
			p := &printer{w: cmd.OutOrStdout()}
			if err := f.client().Pull(cmd.Context(), args[0], progressLines(p, args[0])); err != nil {
				return f.pullFailure(err)
			}
			return p.err
		},
	}
}

// progressLines prints the events of a pull: one line when a file is being resolved, one for every five percent
// that a download advances, one when it is verified, and one when it is in place. The last event of a pull names
// no file, and then the line names what was asked for. A failure is not printed here, the pull returns it.
func progressLines(p *printer, ref string) func(api.PullEvent) {
	lastStep := map[string]int64{}
	return func(e api.PullEvent) {
		switch e.Stage {
		case "resolving":
			p.printf("resolving %s\n", e.File)
		case "downloading":
			if e.Total <= 0 {
				return
			}
			if step := e.Bytes * 20 / e.Total; step != lastStep[e.File] {
				lastStep[e.File] = step
				p.printf("downloading %s %s / %s\n", e.File, size(e.Bytes), size(e.Total))
			}
		case "verifying":
			p.printf("verifying %s\n", e.File)
		case "done":
			name := e.File
			if name == "" {
				name = ref
			}
			p.printf("done %s\n", name)
		}
	}
}

// pullFailure explains a pull that did not finish. The checks run in an order that matters. A canceled context
// comes first, because a pull that is interrupted midway matches the early end of the stream as well, and the person
// who stopped it needs no word about the daemon. Then a refusal by Hugging Face for lack of a token, which gets a
// hint at the token setting and never the token. Then a stream that was cut or ended early, which leaves the partial
// download in place, so that running the same pull again resumes it. Whatever else the daemon reported stays as it
// said it, and a dial that failed is the one case of a daemon that is not running.
func (f *globalFlags) pullFailure(err error) error {
	var reported *client.Error
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("the pull was interrupted (run the same pull again to resume): %w", context.Canceled)
	case errors.As(err, &reported) && hfRefused.MatchString(reported.Message):
		return fmt.Errorf("%w (a gated or private model needs a Hugging Face token: set hf_token in %s, or "+
			"TARAENGINE_HF_TOKEN, and restart `taraengine serve`)", err, configPath(f))
	case errors.Is(err, client.ErrIncompleteStream):
		return fmt.Errorf("%w (run the same pull again to resume; `taraengine doctor` shows whether taraengine is "+
			"still running)", err)
	}
	return f.explain(err)
}
