#!/bin/bash
# daily-report.sh - Generate daily reports for all configured clusters

echo "OpsCart Daily Report Generation"
echo "=================================="
echo ""

# Check if opscart-scan exists
if [ ! -f "./opscart-scan" ]; then
    echo "❌ opscart-scan not found. Build it first:"
    echo "   go build -o opscart-scan cmd/opscart-scan/main.go"
    exit 1
fi

# Get today's date for reporting
TODAY=$(date +%Y-%m-%d)
TIMESTAMP=$(date +"%H:%M:%S")

echo "Started at: $TIMESTAMP"
echo ""

# Generate HTML reports
echo "Generating HTML reports..."
./opscart-scan report --all-clusters --format=html --monthly-cost 5000

if [ $? -ne 0 ]; then
    echo "❌ HTML generation failed"
    exit 1
fi

echo ""

# Generate JSON reports (for automation)
echo "📄 Generating JSON reports..."
./opscart-scan report --all-clusters --format=json --monthly-cost 5000

if [ $? -ne 0 ]; then
    echo "❌ JSON generation failed"
    exit 1
fi

echo ""
echo "All reports generated successfully!"
echo ""

# Show summary
echo "📁 Reports saved to: reports/$TODAY/"
echo ""
echo "📊 Generated files:"
ls -lh reports/$TODAY/ | tail -n +2

echo ""
echo "💾 Total size:"
du -sh reports/$TODAY/

# Optional: Send notification (uncomment to enable)
# echo "Daily cluster reports generated at $TIMESTAMP" | \
#   mail -s "OpsCart Daily Report - $TODAY" team@company.com

# Optional: Copy to shared drive (uncomment and modify path)
# echo ""
# echo "📤 Copying to shared drive..."
# cp -r reports/$TODAY /mnt/shared-drive/opscart-reports/

echo ""
echo "Daily report generation complete!"