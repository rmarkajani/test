#!/bin/bash

cluster_name=$1
artifactory=$2
aws_account_number=$4
karpenter_version=$5
karpenter_role_name=$6
karpenter_node_role_name=$7
environment=$8
uninstall=$9

update-kubeconfig() {
    echo "Updating kubeconfig for cluster: $cluster_name"

    aws eks update-kubeconfig --name $cluster_name
}

git-config() {
    git config --global url."https://${GH_USER}:${GH_TOKEN}@github.com".insteadOf https://github.com
}

clone-karpenter() {
    version=$1

    git clone https://github.com/cobank-acb/cpe-helm-karpenter.git
    cd ./cpe-helm-karpenter
    git fetch --all
    git checkout $version
    ls -al
}

uninstall-karpenter() {

    git-config
    update-kubeconfig --name $cluster_name

    # check if the kubeconfig update was successful
    if [[ $? -ne 0 ]]; then
        echo "Failed to update kubeconfig for Karpenter. Please check your AWS credentials and cluster name."
        exit 1
    fi

    echo -e "\033[36mUninstalling Karpenter..."
    helm uninstall karpenter -n karpenter
    kubectl delete namespace karpenter --ignore-not-found
}

install-karpenter() {

    if [[ "$uninstall" == "true" ]]; then
        echo "Uninstall flag is set to 'true'. Uninstalling Karpenter..."
        uninstall-karpenter "$cluster_name"
        return
    fi

    git-config
    update-kubeconfig --name $cluster_name

    # check if the kubeconfig update was successful
    if [[ $? -ne 0 ]] 
    then
        echo "Failed to update kubeconfig for Karpenter2. Please check your AWS credentials and cluster name."
        exit 1
    fi

    # check that the node classes that karpenter would create exist
    NODECLASS_CHECK=$(kubectl get ec2nodeclass system 2> /dev/null)
    
    if [[ $? -eq 1 ]]
    then
        KARPENTER_CHECK=$(kubectl get deployment karpenter -n karpenter -o=jsonpath='{.status.availableReplicas}')
        if [[ $KARPENTER_CHECK -lt "1" ]]
        then
            echo -e "\033[36mInstalling pre-req karpenter..."
            clone-karpenter $karpenter_version
            helm upgrade karpenter . \
                -n karpenter \
                --install \
                --create-namespace \
                --wait \
                -f values.yaml \
                --set clusterName=$cluster_name \
                --set karpenter.controller.image.repository=$artifactory/karpenter/controller \
                --set-string karpenter.controller.image.digest="" \
                --set karpenter.replicas=3 \
                --set karpenter.serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::$aws_account_number:role/$karpenter_role_name \
                --set karpenter.settings.clusterName=$cluster_name \
                --set karpenter.settings.interruptionQueue=karpenter-$cluster_name \
                --set systemPool[0].region=$aws_region \
                --set systemClass[0].region=$aws_region \
                --set systemClass[0].environment=$environment \
                --set systemClass[0].nodeRole=$karpenter_node_role_name \
                --set-string karpenter.settings.reservedENIs="8" \
                --timeout 2m
        else
            echo -e "\033[32m karpenter is already installed!"
            exit 0
        fi
    else 
        echo -e "\033[32m karpenter is already installed!"
        exit 0
    fi
}

cluster_name=$1
artifactory=$2
aws_account_number=$4
karpenter_version=$5
karpenter_role_name=$6
karpenter_node_role_name=$7
environment=$8

install-karpenter $cluster_name $artifactory $aws_account_number $karpenter_version $karpenter_role_name $karpenter_node_role_name $environment