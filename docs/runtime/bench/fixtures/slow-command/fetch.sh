#!/bin/sh
# Pretends to fetch data: takes 120 s, then writes data.txt.
sleep 120
i=1; n=$(( $$ % 40 + 17 ))
: > data.txt
while [ $i -le $n ]; do echo "row $i" >> data.txt; i=$((i+1)); done
