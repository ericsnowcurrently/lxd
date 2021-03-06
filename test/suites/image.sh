#!/bin/sh

test_image_expiry() {
  ensure_import_testimage

  if ! lxc remote list | grep -q l1; then
    lxc remote add l1 "${LXD_ADDR}" --accept-certificate --password foo
  fi
  if ! lxc remote list | grep -q l2; then
    lxc remote add l2 "${LXD2_ADDR}" --accept-certificate --password foo
  fi
  lxc init l1:testimage l2:c1
  fp=$(lxc image info testimage | awk -F: '/^Fingerprint/ { print $2 }' | awk '{ print $1 }')
  [ ! -z "${fp}" ]
  fpbrief=$(echo "${fp}" | cut -c 1-10)

  lxc image list l2: | grep -q "${fpbrief}"

  lxc remote set-default l2
  lxc config set images.remote_cache_expiry 0
  lxc remote set-default local

  ! lxc image list l2: | grep -q "${fpbrief}"

  lxc delete l2:c1

  # rest the default expiry
  lxc remote set-default l2
  lxc config set images.remote_cache_expiry 10
  lxc remote set-default local
}
