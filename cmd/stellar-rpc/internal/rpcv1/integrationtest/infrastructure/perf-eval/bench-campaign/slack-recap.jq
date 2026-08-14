# Reads converted results-site run JSON and emits {header, lines, footer} or null.
# Tiers are red past block time, orange past target, and green under target.
    def commafy: tostring | . as $s | ($s | length) as $l
      | if $l <= 3 then $s else ($s[0:$l-3] | commafy) + "," + $s[$l-3:] end;
    . as $root
    | .campaign.phase as $ph
    | ((.campaign.phase_targets // []) | map(select(.phase == $ph)) | first) as $t
    | ($t.ingest_p99_target_ns // null) as $target
    | ($t.block_time_ns // $root.checks.interval_ns // null) as $block
    | ($root.ingest_hot // {}) as $ih
    | if $ph == null or $target == null or $block == null or ($ih | length) == 0 then null else
        ( [ ($root.dataset.unit_order // ($ih | keys))[]
            | . as $u
            | ($ih[$u].driver.ingest_total.p99.m // null) as $p99
            | select($p99 != null)
            | (try ($u | capture("-(?<txpl>[0-9]+)-c[0-9]+$").txpl | tonumber) catch null) as $txpl
            | ((($t.workloads // []) | map(select(.tx_per_ledger == $txpl)) | first | .name) // $u) as $name
            | {name: $name, txpl: $txpl, p99: $p99}
          ] | sort_by(-.p99) ) as $rows
        | if ($rows | length) == 0 then null else
            (($target / 1e6) | round) as $target_ms
            | (($block / 1e6) | round) as $block_ms
            | {
                header: "*Ingestion p99 vs the Phase \($ph) target (\($target_ms) ms)*",
                lines: [ $rows[]
                  | ((.p99 / 1e6) | round) as $ms
                  | (((.p99 / $target * 10) | round) / 10) as $mult
                  | (if $ms >= 1000 then "\((($ms / 100) | round) / 10) s" else "\($ms) ms" end) as $disp
                  | (if .txpl == null then "" else " · \(.txpl | commafy) tx/ledger" end) as $load
                  | (if .p99 > $block then "🔴 \(.name)\($load) — *\($disp)* (\($mult)× target · past the \($block_ms) ms block time)"
                     elif .p99 > $target then "🟠 \(.name)\($load) — *\($disp)* (\($mult)× target)"
                     else "🟢 \(.name)\($load) — *\($disp)* (under target)" end)
                ],
                footer: "_median of \($root.campaign.reps // 1) run\(if ($root.campaign.reps // 1) == 1 then "" else "s" end) · block time \($block_ms) ms_"
              }
          end
      end
