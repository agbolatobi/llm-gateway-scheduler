"""Export a benchmark run's saved AIPerf results to PDF; never sends requests."""

import argparse
from datetime import datetime, timezone
from html import escape
from io import BytesIO
import math
from pathlib import Path
import sys

from .report_data import (
    METRICS,
    STATS,
    read_object,
    contained_path,
    metric_rows,
    memory_rows,
    load_report,
)


def require_pdf_support():
    try:
        import reportlab  # noqa: F401
    except ImportError as error:
        raise RuntimeError(
            "PDF export requires ReportLab. Activate the root .venv and run "
            "python -m pip install -r requirements.txt"
        ) from error


def number(value):
    if type(value) not in (int, float) or not math.isfinite(value):
        return "N/A"
    # Keep large millisecond values readable within narrow statistics columns.
    if abs(value) >= 1_000_000:
        return f"{value:.1e}"
    if abs(value) >= 10_000:
        return f"{value:,.0f}"
    return f"{value:,.4g}"


def export_pdf(run_directory, output=None):
    require_pdf_support()
    data = load_report(run_directory)
    destination = Path(output).resolve() if output else data["root"] / "report.pdf"
    if destination.suffix.lower() != ".pdf":
        raise ValueError("PDF output must have a .pdf extension")
    if destination.exists():
        raise FileExistsError(
            f"Refusing to overwrite {destination}; choose --output with a new name"
        )

    import reportlab
    from reportlab.lib import colors
    from reportlab.lib.enums import TA_RIGHT
    from reportlab.lib.pagesizes import A4
    from reportlab.lib.styles import ParagraphStyle
    from reportlab.pdfbase import pdfmetrics
    from reportlab.pdfbase.ttfonts import TTFont
    from reportlab.platypus import (
        KeepTogether,
        PageBreak,
        Paragraph,
        SimpleDocTemplate,
        Spacer,
        Table,
        TableStyle,
    )

    fonts = Path(reportlab.__file__).parent / "fonts"
    for name, filename in (("ReportBody", "Vera.ttf"), ("ReportBold", "VeraBd.ttf")):
        if name not in pdfmetrics.getRegisteredFontNames():
            pdfmetrics.registerFont(TTFont(name, str(fonts / filename)))
    supported = pdfmetrics.getFont("ReportBody").face.charWidths
    navy = colors.HexColor("#17324D")
    teal = colors.HexColor("#167D8D")
    muted = colors.HexColor("#506170")
    body = ParagraphStyle(
        "body",
        fontName="ReportBody",
        fontSize=9,
        leading=13,
        textColor=navy,
        spaceAfter=7,
        splitLongWords=True,
    )
    small = ParagraphStyle("small", parent=body, fontSize=8, leading=11)
    heading = ParagraphStyle(
        "heading", parent=body, fontName="ReportBold", fontSize=17, leading=22, spaceAfter=14
    )
    section = ParagraphStyle(
        "section",
        parent=body,
        fontName="ReportBold",
        fontSize=11,
        leading=15,
        textColor=teal,
        spaceBefore=10,
        keepWithNext=True,
    )
    head = ParagraphStyle("head", parent=small, fontName="ReportBold", textColor=colors.white)
    numeric = ParagraphStyle("numeric", parent=small, alignment=TA_RIGHT)

    def paragraph(value, style=body):
        text = str(value if value is not None else "N/A")
        # Escape markup; never interpret model names/paths as links or images.
        # Unsupported glyphs are shown as explicit escapes instead of black boxes.
        text = "".join(
            (
                character
                if ord(character) in supported or character == "\n"
                else character.encode("unicode_escape").decode("ascii")
            )
            for character in text
        )
        return Paragraph(escape(text).replace("\n", "<br/>"), style)

    width = A4[0] - 96

    def table(rows, widths):
        rendered = [[paragraph(cell, head) for cell in rows[0]]]
        for row in rows[1:]:
            rendered.append(
                [
                    paragraph(cell, numeric if index >= 2 else small)
                    for index, cell in enumerate(row)
                ]
            )
        result = Table(rendered, colWidths=widths, repeatRows=1, hAlign="LEFT")
        result.setStyle(
            TableStyle(
                [
                    ("BACKGROUND", (0, 0), (-1, 0), navy),
                    (
                        "ROWBACKGROUNDS",
                        (0, 1),
                        (-1, -1),
                        [colors.HexColor("#F0F5F8"), colors.white],
                    ),
                    ("VALIGN", (0, 0), (-1, -1), "TOP"),
                    ("LEFTPADDING", (0, 0), (-1, -1), 8),
                    ("RIGHTPADDING", (0, 0), (-1, -1), 8),
                    ("TOPPADDING", (0, 0), (-1, -1), 7),
                    ("BOTTOMPADDING", (0, 0), (-1, -1), 7),
                    ("LINEBELOW", (0, -1), (-1, -1), 0.5, colors.HexColor("#C7D3DE")),
                ]
            )
        )
        return result

    run = data["run"]
    settings = run["settings"]
    manifest = data["manifest"]
    story = [
        paragraph("LOCAL INFERENCE / BENCHMARK REPORT", section),
        paragraph("Inference performance", heading),
        paragraph(f"Run status: {run.get('status', 'unknown')}"),
        paragraph(
            "A saved-results summary. No inference is performed during PDF export.", small
        ),
    ]
    if run.get("status") != "completed":
        story.append(
            paragraph(
                "PARTIAL RUN - use these results for diagnosis, not as a completed baseline.",
                section,
            )
        )
    metadata = [
        ("Run", data["root"].name),
        ("Started (UTC)", run.get("created_at")),
        ("Model alias", settings.get("model")),
        ("Endpoint", settings.get("base_url")),
        ("AIPerf version", run.get("aiperf_version")),
        ("Concurrency", manifest.get("concurrency", settings.get("concurrency"))),
        ("Repetitions per case", manifest.get("repetitions", settings.get("repetitions"))),
        ("Warm-ups per case", run.get("warmup_requests_per_case")),
        ("Temperature", settings.get("temperature")),
        (
            "Cross-request prompt cache",
            settings.get("prompt_cache", "unspecified (legacy run)"),
        ),
        ("Planned measured requests", run.get("measured_request_count")),
    ]
    story.append(table([["Setting", "Value"], *metadata], [155, width - 155]))
    story += [paragraph("Case coverage", section)]
    coverage = [["Case", "Status", "Measured", "Errors", "Warm-up"]]
    for case in data["cases"]:
        counts = case["outcomes"] or {}
        coverage.append(
            [
                case["id"],
                case["status"],
                *(
                    number(counts.get(field_name))
                    for field_name in ("measured", "errors", "warmup")
                ),
            ]
        )
    story.append(table(coverage, [width - 282, 93, 63, 63, 63]))
    story += [
        Spacer(1, 12),
        paragraph(
            "Coverage uses completion-verified records when available, otherwise native AIPerf records. "
            "Warm-ups are separate. Missing data is N/A. Case status is copied from run.json.",
            small,
        ),
    ]
    story.append(
        KeepTogether(
            [
                paragraph("Provenance", section),
                paragraph(f"Source SHA-256: {run.get('source_sha256', 'N/A')}", small),
                paragraph(f"Run directory: {data['root']}", small),
                paragraph("Generated (UTC): " + datetime.now(timezone.utc).isoformat(), small),
            ]
        )
    )
    # A single cross-case view supports the MEAS-02 comparisons without repeating
    # another five-row statistics table on every case page.
    story += [
        PageBreak(),
        paragraph("PROMPT CACHE / BACKEND PHASES", section),
        paragraph("Prompt processing and generation", heading),
        paragraph(
            "P50 values for successful measured requests; warm-ups excluded. "
            "Input is actual server usage, not the requested target. "
            "New/cached/output counters and phase times are reported by llama.cpp. "
            "Missing backend counters remain N/A.",
            small,
        ),
    ]
    phases = [["Case", "Input", "New", "Cached", "Output", "Prompt ms", "Decode ms"]]
    for case in data["cases"]:
        phases.append(
            [
                case["id"],
                *(
                    number(case["backend"].get(key, {}).get("p50"))
                    for key in (
                        "input_tokens",
                        "backend_prompt_tokens",
                        "cached_input_tokens",
                        "output_tokens",
                        "backend_prompt_ms",
                        "backend_decode_ms",
                    )
                ),
            ]
        )
    story.append(table(phases, [width - 330, 45, 45, 55, 55, 65, 65]))
    story.append(paragraph("Observed cache coverage", section))
    for case in data["cases"]:
        verification = case["cache_verification"]
        if verification is None:
            text = "unavailable"
        else:
            text = (
                f"{verification['uncached_requests']} uncached, "
                f"{verification['cached_requests']} with cache hits, "
                f"{verification['unverified_requests']} unverified successful requests"
            )
        story.append(paragraph(f"{case['id']}: {text}.", small))
    story.append(
        paragraph(
            "off requests no cross-request reuse; new runs fail verification if counters show hits "
            "or cannot establish that new + cached tokens equals actual input with zero cached. "
            "reuse allows hits but does not guarantee them. Model warm-up is separate from prefix "
            "reuse. For input-length comparisons fix the output cap; for output-length comparisons "
            "fix the prompt. Do not equate client TTFT with backend prompt_ms.",
            small,
        )
    )
    for case in data["cases"]:
        spec = case["spec"]
        story += [
            PageBreak(),
            paragraph("CASE RESULTS", section),
            paragraph(case["id"], heading),
            paragraph(f"Status: {case['status']} | Prompt: {spec.get('prompt_id', 'N/A')}"),
            paragraph(
                f"Output cap: {spec.get('max_tokens', 'N/A')} tokens | "
                f"Target input: {spec.get('target_input_tokens', 'N/A')} tokens (unverified)",
                small,
            ),
        ]
        for warning in case["warnings"]:
            story.append(paragraph("Warning: " + warning, small))
        rows = [["Metric", "Unit", "Mean", "P50", "P95", "P99"]]
        rows += [
            [label, unit, *(number(display_value) for display_value in stats)]
            for label, unit, *stats in case["rows"]
        ]
        story.append(table(rows, [width - 315, 75, 60, 60, 60, 60]))
        memory = case["memory"]
        memory_table = [["Memory (MiB)", "Baseline", "Case peak", "Samples", "Status"]]
        memory_table += [
            [label, number(baseline), number(peak), number(count), status]
            for label, baseline, peak, count, status, _ in case["memory_rows"]
        ]
        story.append(
            KeepTogether(
                [
                    paragraph("Sampled server memory", section),
                    table(memory_table, [width - 295, 65, 75, 65, 90]),
                ]
            )
        )
        for label, _, _, _, _, reason in case["memory_rows"]:
            if reason:
                story.append(paragraph(f"{label}: {reason}", small))
        if memory:
            story.append(
                paragraph(
                    f"PID: {memory.get('server_pid')} | Requested interval: "
                    f"{memory.get('sampling_interval_seconds')} s. Maximum observed samples; "
                    "spikes may be missed. Baseline is pre-case, not guaranteed idle. Case window "
                    "includes AIPerf setup, warm-ups and teardown, excluding model startup. "
                    "Request-window peaks are in request_metrics.jsonl/csv; overlapping requests "
                    "share process observations, not attributable allocations.",
                    small,
                )
            )
        else:
            story.append(
                paragraph("Memory unavailable: memory_not_collected (older run).", small)
            )
        story += [
            paragraph("Source files", section),
            paragraph(
                f"{case['id']}/profile_export_aiperf.json\n{case['id']}/profile_export.jsonl"
                + (f"\n{case['id']}/request_metrics.jsonl" if case["completion"] else "")
                + (f"\n{case['id']}/memory_summary.json" if memory else ""),
                small,
            ),
        ]
    # Shared definitions belong once, keeping case pages focused on results.
    story += [
        PageBreak(),
        paragraph("READING THE REPORT", section),
        paragraph("Measurement notes", heading),
    ]
    for note in (
        "First generated text and completion E2E use the original monotonic timestamps. "
        "E2E ends at the first SSE [DONE] event. Their statistics include only successful "
        "profiling requests and use linear interpolation at (n - 1) * p.",
        "Other values and units come from AIPerf's profiling summary. Its request latency "
        "ends at the last content-bearing response. No cross-case averaging is performed. "
        "Scalar metrics may only have a Mean value.",
        "AIPerf TTFT uses its parsed content definition. First generated text uses non-empty "
        "delta.content. Inter-token latency is a derived average, not a timestamp for every token.",
        "Small samples do not establish reliable tail latency. Inspect request errors and "
        "raw records before comparing runs. Output caps are not guaranteed output lengths.",
        "Actual input/output token counts come from server usage. RSS includes resident "
        "shared/file-backed pages, not virtual size or KV-cache allocation. VRAM uses "
        "per-process used memory, not device-wide totals or allocator-reserved memory.",
        "Memory peaks are sampled observations, not guaranteed maxima. Missing counters are "
        "N/A with a reason; partial coverage retains observed values and error counts. "
        "Request-window peaks can share readings when requests overlap, and do not measure "
        "isolated per-request allocation. WSL process VRAM is marked unsupported.",
    ):
        story.append(paragraph(note, body))

    def footer(canvas, document):
        canvas.saveState()
        canvas.setStrokeColor(teal)
        canvas.line(48, 42, A4[0] - 48, 42)
        canvas.setFillColor(muted)
        canvas.setFont("ReportBody", 8)
        canvas.drawString(48, 28, "LLM Gateway Scheduler | AIPerf results")
        canvas.drawRightString(A4[0] - 48, 28, f"Page {document.page}")
        canvas.restoreState()

    buffer = BytesIO()
    document = SimpleDocTemplate(
        buffer,
        pagesize=A4,
        leftMargin=48,
        rightMargin=48,
        topMargin=42,
        bottomMargin=57,
        title="Local LLM benchmark report",
        author="LLM Gateway Scheduler",
    )
    document.build(story, onFirstPage=footer, onLaterPages=footer)
    destination.parent.mkdir(parents=True, exist_ok=True)
    with destination.open("xb") as stream:
        stream.write(buffer.getvalue())
    return destination


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "run_directory", type=Path, help="Existing directory containing run.json"
    )
    parser.add_argument(
        "--output", type=Path, help="New PDF path (default: RUN_DIRECTORY/report.pdf)"
    )
    args = parser.parse_args(argv)
    try:
        print(f"PDF report: {export_pdf(args.run_directory, args.output)}")
        return 0
    except (OSError, ValueError, RuntimeError) as error:
        print(f"PDF export failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
