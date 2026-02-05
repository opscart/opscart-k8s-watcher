#!/bin/bash
# cleanup-reports.sh - Delete reports older than specified days

# Configuration
DAYS=${1:-30}  # Default: 30 days (can override with argument)

echo "Cleaning up reports older than $DAYS days..."

# Check if reports directory exists
if [ ! -d "reports" ]; then
    echo "❌ No reports directory found"
    exit 0
fi

# Count folders to delete
COUNT=$(find reports/ -type d -name "20*" -mtime +$DAYS 2>/dev/null | wc -l)

if [ $COUNT -eq 0 ]; then
    echo "No old reports to delete"
    exit 0
fi

echo "📁 Found $COUNT date folders to delete"

# Delete old reports
find reports/ -type d -name "20*" -mtime +$DAYS -exec rm -rf {} \; 2>/dev/null

echo "Cleanup complete!"
echo ""

# Show remaining reports
echo "Remaining reports:"
if [ -d "reports" ]; then
    du -sh reports/*/ 2>/dev/null | sort -rh | head -10
fi

echo ""
echo "Total reports disk usage:"
du -sh reports/