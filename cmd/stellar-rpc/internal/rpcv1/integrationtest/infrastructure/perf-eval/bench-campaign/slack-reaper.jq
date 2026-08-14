# Inputs: $reaped, $untagged, $run_url, and $repo.
# Emits one reaper-warning Slack attachment.
    def button($t; $u): {type: "button", text: {type: "plain_text", text: $t, emoji: true}, url: $u};
    ($reaped | length) as $n
    | ($untagged | split(" ") | map(select(. != ""))) as $loose
    | {
        attachments: [{
          color: "#ecb22e",
          fallback: "⚠️ bench reaper terminated past-deadline box(es): \($reaped | map(.id) | join(" ")) · \($run_url)",
          blocks: (
            [{type: "header", text: {type: "plain_text",
              text: "⚠️ Reaper terminated \($n) past-deadline box\(if $n == 1 then "" else "es" end)", emoji: true}}]
            + [{type: "section", text: {type: "mrkdwn", text: (
                ( $reaped | map(
                    (if .overdueMin >= 90 then "\((.overdueMin / 6 | round) / 10) h" else "\(.overdueMin) min" end) as $late
                    | (if (.runId // "") != "" then "campaign run <\($repo)/actions/runs/\(.runId)|\(.runId)>" else "campaign run unknown" end) as $src
                    | "• `\(.id)` — \($src), deadline passed *\($late)* ago"
                  ) )
                + ( $loose | map("• `\(.)` has no deadline tag — left alone (hand-launched?)") )
                | join("\n")
              )}}]
            + (if $run_url != "" then
                [{type: "actions", elements: [button("Reaper run"; $run_url)]}] else [] end)
            + [{type: "context", elements: [{type: "mrkdwn",
                text: "A reap means a campaign box outlived every in-band ceiling — worth a human look."}]}]
          )
        }]
      }
