# Function to show ticker (full cycle)
display_ticker() {
  animation='/ - \ |'
  for c in $loading_animation; do
    sleep 0.2
    /bin/echo -e -n "$c" "\r"
  done
}

# Function to wait until next epoch
sleep_until_next_epoch() {
  epoch_start=$(lavad query epochstorage show-epoch-details | grep "startBlock: ")
  echo "Waiting for the next epoch for the changes to be active"
  loading_animation='/ - \ |'
  while true; do
    epoch_now=$(lavad query epochstorage show-epoch-details | grep "startBlock: ")
    display_ticker
    if [ "$epoch_start" != "$epoch_now" ]; then
      echo "finished waiting"
      break
    fi
  done
}

# Function to wait until next block
wait_next_block() {
  current=$( lavad q block | jq .block.header.height)
  echo "waiting for next block $current"
  while true; do
    display_ticker
    new=$( lavad q block | jq .block.header.height)
    if [ "$current" != "$new" ]; then
      echo "finished waiting at block $new"
      break
    fi
  done
}

current_block() {
  current=$(lavad q block | jq -r .block.header.height)
  echo $current
}

# function to wait until count ($1) blocks complete
wait_count_blocks() {
  for i in $(seq 1 $1); do
    wait_next_block
  done
}

# Function to check if a command is available
command_exists() {
  command -v "$1" >/dev/null 2>&1
}

# Function to check the Go version is at least 1.21
check_go_version() {
  local GO_VERSION=1.21

  if ! command_exists go; then
    return 1
  fi

  go_version=$(go version)
  go_version_major_full=$(echo "$go_version" | awk '{print $3}')
  go_version_major=${go_version_major_full:2}
  result=$(echo "${go_version_major}-$GO_VERSION" | bc -l)
  if [ "$result" -ge 0 ]; then
    return 0
  fi

  return 1
}

# Function to find the latest vote
latest_vote() {
  # Check if jq is not installed
  if ! command_exists yq; then
      echo "yq not found. Please install yq using the init_install.sh script or manually."
      exit 1
  fi
  lavad q gov proposals 2> /dev/null | yq eval '.proposals[].id'  | wc -l
}

create_health_config() {
  local source_file="$1"
  local destination_file="${source_file%.*}_gen.yml"
  local subscription_address="$2"
  local provider_address_1="$3"
  local provider_address_2="$4"
  local provider_address_3="$5"

  # Use awk to find the line number of the comment
  local comment_line_number=$(awk '/#REPLACED/ {print NR; exit}' "$source_file")

  # If the comment is found, create a new file with the updated content
  if [ -n "$comment_line_number" ]; then
    # Use awk to update the content and create a new file
    awk -v line="$comment_line_number" -v sub_addr="$subscription_address" \
        -v prov_addr_1="$provider_address_1" -v prov_addr_2="$provider_address_2" \
        -v prov_addr_3="$provider_address_3" \
        '{
            if (NR <= line) {
                print;
            } else if (NR == line + 1) {
                print "subscription_addresses:";
                print "  - " sub_addr;
                print "provider_addresses:";
                print "  - " prov_addr_1;
                print "  - " prov_addr_2;
                print "  - " prov_addr_3;
            }
        }' "$source_file" > "$destination_file"

    echo "File $destination_file created successfully."
  else
    echo "Comment #REPLACED not found in the file."
    exit 1
  fi
}