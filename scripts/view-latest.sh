#!/bin/bash
# view-latest.sh - Open the most recent HTML report

# Find the most recent HTML report
LATEST=$(find reports/ -name "*.html" -type f -print0 2>/dev/null | xargs -0 ls -t 2>/dev/null | head -1)

if [ -z "$LATEST" ]; then
    echo "❌ No HTML reports found in reports/ directory"
    echo ""
    echo "Generate a report first:"
    echo "  ./opscart-scan report --cluster opscart --format=html"
    exit 1
fi

echo "Opening latest report: $LATEST"
echo ""

# Detect OS and open appropriately
if [[ "$OSTYPE" == "darwin"* ]]; then
    # macOS
    open "$LATEST"
elif [[ "$OSTYPE" == "linux-gnu"* ]]; then
    # Linux
    if command -v xdg-open &> /dev/null; then
        xdg-open "$LATEST"
    else
        echo "Report found: $LATEST"
        echo "Open it manually in your browser"
    fi
else
    # Windows or other
    echo "Report found: $LATEST"
    echo "Open it manually in your browser"
fi